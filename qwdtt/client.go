package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const (
	// maxWorkers and the rounding to whole groups follow upstream's client main.
	maxWorkers     = 108
	statusInterval = 2 * time.Second
	peerAttempts   = 3
	peerTimeout    = 10 * time.Second
)

var errClosed = errors.New("qwdtt: stopped")

func start(_ context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	cfg, err := decodeLink(rt.Link)
	if err != nil {
		return nil, err
	}
	rt.Log.AddSecret(secretsOf(cfg, rt.Link)...)
	wrapKey, err := deriveWrapKey(cfg.Pass)
	if err != nil {
		return nil, err
	}
	// Upstream keeps its VK device profile in the working directory.
	if rt.DataDir != "" {
		if err := os.Chdir(rt.DataDir); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cl := &client{
		rt: rt, cfg: cfg, wrapKey: wrapKey, deviceID: deviceIDFor(cfg.Pass, rt.DataDir),
		ctx: ctx, cancel: cancel, pipe: newWGPipe(),
		ready: make(chan struct{}), failed: make(chan struct{}),
	}
	log.SetFlags(0)
	log.SetOutput(&upstreamLog{w: rt.Log.Writer(sdk.LevelDebug), note: cl.noteLog})
	useEgress(ctx, rt.Egress)
	setVkAuthMode("anonymous")
	setVkAnonPath("vkcalls")
	setCaptchaMode("auto")
	if upstreamStdout != nil {
		go cl.readStdout(upstreamStdout)
	}
	go cl.run()
	return cl, nil
}

// deviceIDFor is the device id the server binds a password to (upstream sends Android's ANDROID_ID). It is
// derived from the machine id, so every run on one machine is one device, and from the password, so servers
// cannot link their users.
func deviceIDFor(pass, dataDir string) string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				sum := sha256.Sum256([]byte("qwdtt-device\x00" + id + "\x00" + pass))
				return hex.EncodeToString(sum[:8])
			}
		}
	}
	path := filepath.Join(dataDir, "device-id")
	if dataDir != "" {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) == 16 {
			return strings.TrimSpace(string(b))
		}
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	if dataDir != "" {
		_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
	}
	return id
}

// client is the Proxy the front serves: CONNECT and UDP flows enter the userspace WireGuard netstack, whose
// packets upstream's workers carry over DTLS through VK TURN relays to the server.
type client struct {
	rt       *sdk.Runtime
	cfg      config
	wrapKey  []byte
	deviceID string
	ctx      context.Context
	cancel   context.CancelFunc
	pipe     *wgPipe

	ready  chan struct{}
	failed chan struct{}

	mu       sync.Mutex
	tnet     *netstack.Net
	dev      *device.Device
	err      error
	reason   string
	closed   bool
	failOnce sync.Once
	once     sync.Once
}

func (c *client) fail(err error) {
	c.failOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		closed := c.closed
		c.mu.Unlock()
		close(c.failed)
		if !closed {
			c.rt.Fatal(err)
		}
	})
}

func (c *client) run() {
	c.rt.Status(sdk.StateDegraded, "connecting")
	peer, err := c.resolvePeer()
	if err != nil {
		if c.ctx.Err() == nil {
			c.fail(err)
		}
		return
	}

	disp := NewDispatcher(c.ctx, c.pipe.packetConn(), NewStats())
	defer disp.Shutdown()
	configCh := make(chan string, 1)
	go c.awaitConfig(configCh)
	go c.watch(disp)

	tp := &TurnParams{Hashes: c.cfg.Hashes, WrapKey: c.wrapKey, ObfsMode: normalizeObfsMode("audio")}
	workers := min(c.cfg.Workers, maxWorkers)
	workers = max(workers/workersPerGroup, 1) * workersPerGroup
	groups := workers / workersPerGroup
	c.rt.Log.Infof("server %s, %d hashes, %d workers in %d groups", peer, len(c.cfg.Hashes), workers, groups)

	var pause int32
	var wg sync.WaitGroup
	var prev <-chan struct{}
	id := 1
	for g := range groups {
		var next chan struct{}
		if g < groups-1 {
			next = make(chan struct{})
		}
		ids := make([]int, workersPerGroup)
		for i := range ids {
			ids[i] = id
			id++
		}
		var cc chan<- string
		if g == 0 {
			cc = configCh
		}
		wait, signal := prev, next
		wg.Go(func() {
			var sig chan<- struct{}
			if signal != nil {
				sig = signal
			}
			WorkerGroup(c.ctx, g+1, g, tp, peer, disp, c.cfg.Port, g == 0, cc, ids, &pause, c.deviceID, c.cfg.Pass,
				NewStats(), wait, sig)
		})
		if next != nil {
			prev = next
		}
	}
	wg.Wait()
	if c.ctx.Err() == nil {
		c.fail(c.stopReason())
	}
}

// resolvePeer finds the server's address. It must be the real one: the TURN relay sends to it, so a fake-ip
// answer of the host resolver would strand every packet.
func (c *client) resolvePeer() (*net.UDPAddr, error) {
	host, port, _ := net.SplitHostPort(c.cfg.Peer)
	if ip, err := netip.ParseAddr(host); err == nil {
		return net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, mustPort(port))), nil
	}
	var lastErr error
	for range peerAttempts {
		ctx, cancel := context.WithTimeout(c.ctx, peerTimeout)
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
		cancel()
		if err == nil && len(ips) > 0 {
			return net.UDPAddrFromAddrPort(netip.AddrPortFrom(ips[0].Unmap(), mustPort(port))), nil
		}
		lastErr = err
		if c.ctx.Err() != nil {
			return nil, c.ctx.Err()
		}
		c.rt.Log.Warnf("resolving the server: %v", err)
	}
	return nil, fmt.Errorf("cannot resolve the server: %w", lastErr)
}

func mustPort(s string) uint16 {
	var n uint16
	_, _ = fmt.Sscan(s, &n)
	return n
}

// awaitConfig brings the WireGuard device up on the configuration the server hands the first worker.
func (c *client) awaitConfig(configCh <-chan string) {
	var conf string
	select {
	case conf = <-configCh:
	case <-c.ctx.Done():
		return
	}
	wg, err := parseWgQuick(conf)
	if err != nil {
		c.fail(fmt.Errorf("server WireGuard config: %w", err))
		return
	}
	tunDev, tnet, err := netstack.CreateNetTUN(wg.addresses, wg.dns, wg.mtu)
	if err != nil {
		c.fail(err)
		return
	}
	logger := &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf:   func(format string, args ...any) { c.rt.Log.Debugf("wireguard: "+format, args...) },
	}
	dev := device.NewDevice(tunDev, c.pipe.bind(), logger)
	if err := dev.IpcSet(wg.ipcRequest()); err != nil {
		dev.Close()
		c.fail(fmt.Errorf("wireguard: %w", err))
		return
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		c.fail(fmt.Errorf("wireguard: %w", err))
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		dev.Close()
		return
	}
	c.tnet, c.dev = tnet, dev
	c.mu.Unlock()
	c.rt.Log.Infof("tunnel up (%d addresses, mtu %d)", len(wg.addresses), wg.mtu)
	close(c.ready)
	c.rt.Status(sdk.StateOK, "")
}

// watch reports the node degraded while no worker carries traffic, after two checks in a row.
func (c *client) watch(d *Dispatcher) {
	select {
	case <-c.ready:
	case <-c.ctx.Done():
		return
	}
	t := time.NewTicker(statusInterval)
	defer t.Stop()
	down := 0
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		d.mu.Lock()
		n := len(d.workers)
		d.mu.Unlock()
		if n > 0 {
			down = 0
			c.rt.Status(sdk.StateOK, "")
		} else if down++; down >= 2 {
			c.rt.Status(sdk.StateDegraded, "relays reconnecting")
		}
	}
}

// noteLog keeps the reason a worker gave up, for the fatal event when all have.
func (c *client) noteLog(line string) {
	var reason string
	switch {
	case strings.Contains(line, "FATAL_AUTH"):
		switch {
		case strings.Contains(line, "неверный пароль"):
			reason = "the server refused the password"
		case strings.Contains(line, "истёк"):
			reason = "the password has expired"
		case strings.Contains(line, "другому устройству"):
			reason = "the password is bound to other devices"
		default:
			reason = "the server refused access"
		}
	case strings.Contains(line, "хеш мёртв"):
		reason = "the VK call is gone (dead hash)"
	case strings.Contains(line, "Ошибка кредов"):
		reason = "VK did not give TURN credentials"
	default:
		return
	}
	c.mu.Lock()
	c.reason = reason
	c.mu.Unlock()
}

func (c *client) stopReason() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason != "" {
		return errors.New("all relays stopped: " + c.reason)
	}
	return errors.New("all relays stopped")
}

// readStdout serves what upstream prints for its Android host. A captcha VK's challenge needs a WebView this
// process does not have, so the request is declined at once instead of waiting out upstream's timeout.
func (c *client) readStdout(r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 1<<20)
	for s.Scan() {
		line := s.Text()
		cmd, _, _ := strings.Cut(line, "|")
		switch cmd {
		case "CAPTCHA_SOLVE":
			drainCaptchaResult()
			select {
			case CaptchaResultChan <- "error:no webview":
			default:
			}
			c.rt.Log.Warnf("VK asks for a captcha the automatic solver could not pass")
			select {
			case <-c.ready:
			default:
				c.rt.Status(sdk.StateDegraded, "VK asks for a captcha")
			}
		default:
			c.rt.Log.Debugf("%s", line)
		}
	}
}

func (c *client) waitTunnel(ctx context.Context) (*netstack.Net, error) {
	select {
	case <-c.ready:
		return c.tnet, nil
	case <-c.failed:
		return nil, c.err
	case <-c.ctx.Done():
		return nil, errClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *client) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	tnet, err := c.waitTunnel(ctx)
	if err != nil {
		return nil, err
	}
	return tnet.DialContext(ctx, "tcp", address)
}

func (c *client) DialUDP(ctx context.Context, address string) (net.Conn, error) {
	tnet, err := c.waitTunnel(ctx)
	if err != nil {
		return nil, err
	}
	return tnet.DialContext(ctx, "udp", address)
}

func (c *client) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		dev := c.dev
		c.mu.Unlock()
		c.cancel()
		_ = c.pipe.Close()
		if dev != nil {
			dev.Close()
		}
	})
	return nil
}

// upstreamLog is upstream's log output: every line goes to the redacting logger at debug level, and the
// client notes the ones that explain a stop. The log package writes one whole line per Write.
type upstreamLog struct {
	w    io.Writer
	note func(string)
}

func (l *upstreamLog) Write(p []byte) (int, error) {
	l.note(string(p))
	return l.w.Write(p)
}
