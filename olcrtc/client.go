package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	olcclient "github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

// Liveness follows upstream's mobile defaults; the backoff spaces out rejoins of a room that keeps failing.
const (
	livenessInterval = 10 * time.Second
	livenessTimeout  = 15 * time.Second
	livenessFailures = 4
	minBackoff       = 2 * time.Second
	maxBackoff       = time.Minute
	// stableLink is how long a link must have been up for its end to restart the backoff.
	stableLink = time.Minute
	// dialWait bounds how long a connect waits for a link that is still coming up.
	dialWait = 45 * time.Second
)

var errStopped = errors.New("olcrtc: stopped")

func start(_ context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	cfg, err := decodeLink(rt.Link)
	if err != nil {
		return nil, err
	}
	rt.Log.AddSecret(secretsOf(cfg)...)
	log.SetFlags(0)
	log.SetOutput(rt.Log.Writer(sdk.LevelDebug))
	olcclient.SetVerbose(rt.Log.Enabled(sdk.LevelDebug))
	resolver := useEgress(rt.Egress)

	ccfg := olcclient.Config{
		Transport: cfg.Transport, Provider: cfg.Provider, RoomURL: cfg.Room, KeyHex: cfg.KeyHex,
		Resolver: resolver, TransportOptions: cfg.Options,
		Liveness: olcclient.LivenessConfig{
			Interval: livenessInterval, Timeout: livenessTimeout, Failures: livenessFailures,
		},
	}
	if rt.DataDir != "" {
		ccfg.DeviceIDPath = filepath.Join(rt.DataDir, "device-id")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{rt: rt, ctx: ctx, cancel: cancel, up: make(chan struct{}), done: make(chan struct{})}
	go c.run(olcclient.New(ccfg))
	return c, nil
}

// client is the Proxy the front serves: each CONNECT becomes one stream of the session the room carries.
type client struct {
	rt     *sdk.Runtime
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu     sync.Mutex
	dialer olcclient.Dialer
	// up is closed while dialer is set and replaced when the link drops.
	up chan struct{}
}

func (c *client) run(oc *olcclient.Client) {
	defer close(c.done)
	backoff := minBackoff
	for c.ctx.Err() == nil {
		c.rt.Status(sdk.StateDegraded, "joining the room")
		began := time.Now()
		err := oc.RunDialer(c.ctx, c.setDialer)
		c.clearDialer(nil)
		if c.ctx.Err() != nil {
			return
		}
		if fatal := fatalError(err); fatal != nil {
			c.rt.Fatal(fatal)
			return
		}
		reason := "the call ended"
		if err != nil {
			reason = err.Error()
		}
		if time.Since(began) >= stableLink {
			backoff = minBackoff
		}
		c.rt.Status(sdk.StateDegraded, fmt.Sprintf("%s; rejoining in %s", reason, backoff))
		c.rt.Log.Warnf("olcrtc: %s", reason)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// fatalError picks out the failures a rejoin cannot cure. Carrier failures (provider API, signalling, ICE, the
// server not answering) stay degraded and are retried.
func fatalError(err error) error {
	if errors.Is(err, olcclient.ErrConfig) {
		return err
	}
	return nil
}

func (c *client) setDialer(d olcclient.Dialer) {
	c.mu.Lock()
	c.dialer = d
	close(c.up)
	c.mu.Unlock()
	c.rt.Status(sdk.StateOK, "")
}

// clearDialer drops d, or whatever dialer is set when d is nil.
func (c *client) clearDialer(d olcclient.Dialer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dialer != nil && (d == nil || c.dialer == d) {
		c.dialer = nil
		c.up = make(chan struct{})
	}
}

func (c *client) currentDialer() (olcclient.Dialer, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialer, c.up
}

func (c *client) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("olcrtc: bad port in %q", address)
	}
	wait, cancel := context.WithTimeout(ctx, dialWait)
	defer cancel()
	for {
		d, up := c.currentDialer()
		if d != nil {
			conn, err := d.Dial(ctx, host, port)
			if err == nil {
				return conn, nil
			}
			if !errors.Is(err, olcclient.ErrClientStopped) {
				return nil, err
			}
			c.clearDialer(d)
			continue
		}
		select {
		case <-up:
		case <-c.ctx.Done():
			return nil, errStopped
		case <-wait.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("olcrtc: the tunnel is not up")
		}
	}
}

func (c *client) Close() error {
	c.once.Do(func() {
		c.cancel()
		<-c.done
	})
	return nil
}
