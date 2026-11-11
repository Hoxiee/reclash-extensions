package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/transport/registry"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// tunnelDNS answers the names the front is asked for. Queries go through the tunnel and leave from the exit: the
// host's resolver is ReClash's own (fake-ip) and must not be asked.
const tunnelDNS = "1.1.1.1:53"

// statusInterval is how often the carriers' state is checked; a drop is reported after two checks in a row.
const statusInterval = 2 * time.Second

var errClosed = errors.New("openflux: stopped")

// useEgress sends every socket upstream opens through the Core's egress. It runs before any carrier is built, as
// some clone http.DefaultTransport when they are constructed.
func useEgress(d sdk.Dialer) {
	netbind.SetDial(d.DialContext)
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		// Core's environment may name an HTTP proxy; the egress is the only way out.
		t.Proxy = nil
	}
}

func useLogger(log *sdk.Logger) {
	level, w := utils.LevelOff, log.Writer(sdk.LevelInfo)
	if log.Enabled(sdk.LevelDebug) {
		level, w = utils.LevelDebug, log.Writer(sdk.LevelDebug)
	}
	utils.SetLevel(level)
	utils.SetOutput(w)
}

func start(_ context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	c, err := decodeLink(rt.Link)
	if err != nil {
		return nil, err
	}
	rt.Log.AddSecret(secretsOf(c)...)
	useLogger(rt.Log)
	useEgress(rt.Egress)

	var store *transport.CookieStore
	if rt.DataDir != "" {
		if store, err = transport.NewCookieStore(filepath.Join(rt.DataDir, "cookies.json")); err != nil {
			return nil, err
		}
	}
	m, err := buildSession(c, store, rt)
	if err != nil {
		return nil, err
	}
	cl := &client{rt: rt, trans: m, ready: make(chan struct{}), failed: make(chan struct{}), stop: make(chan struct{})}
	cl.resolver = &net.Resolver{PreferGo: true, Dial: cl.dialDNS}
	go cl.run()
	return cl, nil
}

type sessionSpec struct {
	name, typ, url string
	priority       int
	params         map[string]any
}

// specsOf names a link's carriers the way upstream's ShareSessionSpecs does: cookie exchange with the exit is
// addressed by name.
func specsOf(c share.Config) []sessionSpec {
	specs := make([]sessionSpec, 0, len(c.Transports))
	seen := map[string]int{}
	for _, t := range c.Transports {
		name := t.Name
		if name == "" {
			seen[t.Type]++
			name = t.Type
			if n := seen[t.Type]; n > 1 {
				name = fmt.Sprintf("%s-%d", t.Type, n)
			}
		}
		s := sessionSpec{name: name, typ: t.Type, url: t.URL, priority: t.Priority}
		if t.Type == "direct" {
			s.url = ""
			s.params = map[string]any{"dial": t.Dial}
		}
		specs = append(specs, s)
	}
	return specs
}

// buildSession is upstream's client Session for a link (mobile buildSessionWith, client side): every carrier on
// one negotiated, encrypted session; a single carrier falls back to the classic layering until the exit answers.
func buildSession(c share.Config, store *transport.CookieStore, rt *sdk.Runtime) (*manager.Manager, error) {
	if utils.SecretChars(c.Secret) < utils.MinSecretChars {
		return nil, fmt.Errorf("link: the encryption secret is shorter than %d characters", utils.MinSecretChars)
	}
	specs := specsOf(c)
	sources := make([]transport.ContextSource, len(specs))
	for i, s := range specs {
		sources[i] = transport.ContextSource{Type: s.typ, URL: s.url, Priority: s.priority}
	}
	kdfContext, alternates := transport.KDFContexts(share.ContextOf(c), "", sources)

	caps := transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP | transport.CapabilityICMPErrors
	sess, err := transport.NewSession(transport.PeerParameters{Capabilities: caps, MaxPacketSize: transport.MaxNegotiatedPacket}, false)
	if err != nil {
		return nil, err
	}
	if len(specs) == 1 {
		codec := c.Codec
		if codec == "" {
			codec = transport.CodecBatched
		}
		sess.SetClassic(codec)
	}
	sess.SetAlternateContexts(alternates)

	// The exit may ask a client to start carriers the link does not name; this client runs only what it was given.
	refuse := func(cfg *control.TransportConfig) (transport.Transport, error) {
		return nil, errors.New("dynamic transports are not supported")
	}
	m := manager.New(sess, refuse, c.Secret, kdfContext)
	config := transport.DefaultConfig()
	for _, s := range specs {
		raw, err := registry.New(s.typ, s.url, s.params, registry.Options{Base: config, StrictDirect: true})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		if err := sess.AddTransport(s.name, raw, c.Secret, kdfContext, s.priority); err != nil {
			return nil, err
		}
		provider, _ := raw.(manager.CookieProvider)
		if err := m.Add(s.name, s.typ, raw, s.priority, provider); err != nil {
			return nil, err
		}
		if s.typ != "direct" {
			m.SetURL(s.name, s.url)
			if store != nil {
				if err := m.UseCookieStore(store, s.name, s.url); err != nil {
					rt.Log.Warnf("%s: saved cookies not applied: %v", s.name, err)
				}
			}
		}
		rt.Log.Infof("carrier %s (%s), priority %d", s.name, s.typ, s.priority)
	}
	sess.SetControlHandler(m.DispatchControl)
	m.SetCaptchaNotifier(func(name, _, _, reason string) {
		rt.Log.Warnf("%s: the service asks for a check (%s)", name, reason)
		rt.Status(sdk.StateDegraded, name+": the service asks for a check this client cannot show")
	})
	return m, nil
}

// client is the Proxy the front serves: CONNECT and UDP flows enter the tunnel's gVisor stack.
type client struct {
	rt       *sdk.Runtime
	trans    transport.Transport
	resolver *net.Resolver

	ready  chan struct{}
	failed chan struct{}
	stop   chan struct{}

	mu     sync.Mutex
	tun    *tunnel.TCPTunnel
	err    error
	closed bool
	once   sync.Once
}

// run performs the handshake (Start blocks on the client until the exit answers) and then watches the carriers.
func (c *client) run() {
	c.rt.Status(sdk.StateDegraded, "connecting")
	if err := c.trans.Start(); err != nil {
		c.mu.Lock()
		closed := c.closed
		c.err = err
		c.mu.Unlock()
		close(c.failed)
		if !closed {
			c.rt.Fatal(err)
		}
		return
	}
	tun := tunnel.NewTCPTunnel(c.trans, false)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		tun.Close()
		return
	}
	c.tun = tun
	c.mu.Unlock()
	close(c.ready)
	c.rt.Status(sdk.StateOK, "")

	t := time.NewTicker(statusInterval)
	defer t.Stop()
	down := 0
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		if c.trans.IsConnected() {
			down = 0
			c.rt.Status(sdk.StateOK, "")
		} else if down++; down >= 2 {
			c.rt.Status(sdk.StateDegraded, "carriers reconnecting")
		}
	}
}

func (c *client) waitTunnel(ctx context.Context) (*tunnel.TCPTunnel, error) {
	select {
	case <-c.ready:
		return c.tun, nil
	case <-c.failed:
		return nil, c.err
	case <-c.stop:
		return nil, errClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *client) dialDNS(ctx context.Context, network, _ string) (net.Conn, error) {
	tun, err := c.waitTunnel(ctx)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(network, "tcp") {
		return tun.DialTCP(tunnelDNS)
	}
	return tun.DialUDP(tunnelDNS)
}

// target resolves address to the IPv4 "ip:port" the tunnel carries, names through tunnelDNS.
func (c *client) target(ctx context.Context, address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return "", fmt.Errorf("%s: the tunnel carries IPv4 only", address)
		}
		return address, nil
	}
	ips, err := c.resolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("%s: no IPv4 address", host)
	}
	return net.JoinHostPort(ips[0].Unmap().String(), port), nil
}

func (c *client) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	tun, err := c.waitTunnel(ctx)
	if err != nil {
		return nil, err
	}
	dst, err := c.target(ctx, address)
	if err != nil {
		return nil, err
	}
	return tun.DialTCP(dst)
}

func (c *client) DialUDP(ctx context.Context, address string) (net.Conn, error) {
	tun, err := c.waitTunnel(ctx)
	if err != nil {
		return nil, err
	}
	dst, err := c.target(ctx, address)
	if err != nil {
		return nil, err
	}
	return tun.DialUDP(dst)
}

func (c *client) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		tun := c.tun
		c.mu.Unlock()
		close(c.stop)
		_ = c.trans.Stop()
		if tun != nil {
			tun.Close()
		}
	})
	return nil
}
