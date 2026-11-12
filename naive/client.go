package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2"
	"github.com/bogdanfinn/fhttp/httptrace"
	"github.com/bogdanfinn/tls-client/profiles"
	tls "github.com/bogdanfinn/utls"
)

// profile is the Chrome whose ClientHello and HTTP/2 framing the client copies; naive tracks Chrome stable.
var profile = profiles.Chrome_146

// naive raises Chrome's HTTP/2 receive windows (naive_proxy_bin.cc: kTypicalWindow and twice it for the session),
// which is visible to the server, so the client sends the same values.
const (
	streamWindow  = 64 << 20
	sessionWindow = 128 << 20
)

const refusalGrace = time.Second

// rootCAs replaces the system roots in tests.
var rootCAs *x509.CertPool

type slot struct {
	mu sync.Mutex
	cc *http2.ClientConn
}

type client struct {
	cfg       config
	egress    sdk.Dialer
	rt        *sdk.Runtime
	transport *http2.Transport
	slots     []*slot
	next      atomic.Uint32
	// padding is the type the server last negotiated, -1 before the first tunnel; once known, tunnels open
	// with fastopen like naive's and do not wait for the reply.
	padding atomic.Int32
	closed  atomic.Bool
}

func start(_ context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	cfg, err := decodeLink(rt.Link)
	if err != nil {
		return nil, err
	}
	rt.Log.AddSecret(secretsOf(cfg)...)
	c := newClient(cfg, rt.Egress, rt)
	rt.Log.Infof("naive server %s, %d connection(s)", cfg.server(), cfg.conns)
	return c, nil
}

func newClient(cfg config, egress sdk.Dialer, rt *sdk.Runtime) *client {
	settings := profile.GetSettings()
	settings[http2.SettingInitialWindowSize] = streamWindow
	c := &client{
		cfg:    cfg,
		egress: egress,
		rt:     rt,
		transport: &http2.Transport{
			Settings:           settings,
			SettingsOrder:      profile.GetSettingsOrder(),
			ConnectionFlow:     sessionWindow - 65535,
			HeaderPriority:     profile.GetHeaderPriority(),
			PseudoHeaderOrder:  profile.GetPseudoHeaderOrder(),
			DisableCompression: true,
		},
		slots: make([]*slot, cfg.conns),
	}
	for i := range c.slots {
		c.slots[i] = &slot{}
	}
	c.padding.Store(-1)
	return c
}

func (c *client) status(state, msg string) {
	if c.rt != nil {
		c.rt.Status(state, msg)
	}
}

// conn returns an HTTP/2 connection to the server, dialing one through the egress when the slot has none.
func (c *client) conn(ctx context.Context) (*http2.ClientConn, error) {
	s := c.slots[int(c.next.Add(1))%len(c.slots)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cc != nil && s.cc.CanTakeNewRequest() {
		return s.cc, nil
	}
	if c.closed.Load() {
		return nil, net.ErrClosed
	}
	cc, err := c.dial(ctx)
	if err != nil {
		c.status(sdk.StateDegraded, err.Error())
		return nil, err
	}
	if old := s.cc; old != nil {
		_ = old.Close()
	}
	s.cc = cc
	return cc, nil
}

func (c *client) dial(ctx context.Context) (*http2.ClientConn, error) {
	raw, err := c.egress.DialContext(ctx, "tcp", c.cfg.server())
	if err != nil {
		return nil, err
	}
	uc := tls.UClient(raw, &tls.Config{ServerName: c.cfg.host, RootCAs: rootCAs}, profile.GetClientHelloId(), true, false, false)
	if err := uc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", c.cfg.host, err)
	}
	if p := uc.ConnectionState().NegotiatedProtocol; p != "h2" {
		_ = uc.Close()
		return nil, fmt.Errorf("server %s does not speak HTTP/2 (ALPN %q)", c.cfg.host, p)
	}
	cc, err := c.transport.NewClientConn(uc)
	if err != nil {
		_ = uc.Close()
		return nil, err
	}
	return cc, nil
}

// DialTCP opens one CONNECT stream to address.
func (c *client) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	cc, err := c.conn(ctx)
	if err != nil {
		return nil, err
	}
	known := int(c.padding.Load())
	pr, pw := io.Pipe()
	streamCtx, cancel := context.WithCancel(context.Background())
	req := &http.Request{
		Method:        http.MethodConnect,
		Host:          address,
		Header:        c.requestHeader(known >= 0),
		Body:          pr,
		ContentLength: -1,
	}
	t := &tunnel{pw: pw, cancel: cancel, ready: make(chan struct{}), padding: known}
	req = req.WithContext(httptrace.WithClientTrace(streamCtx, &httptrace.ClientTrace{GotFirstResponseByte: t.responseStarted}))
	go t.roundTrip(c, cc, req)
	if known >= 0 {
		t.writer = t.newWriter(known)
		return t, nil
	}
	select {
	case <-t.ready:
	case <-ctx.Done():
		_ = t.Close()
		return nil, context.Cause(ctx)
	}
	if t.err != nil {
		_ = t.Close()
		return nil, t.err
	}
	t.writer = t.newWriter(t.padding)
	return t, nil
}

func (c *client) requestHeader(fastOpen bool) http.Header {
	h := http.Header{}
	var order []string
	add := func(name, value string) {
		h[name] = []string{value}
		order = append(order, name)
	}
	if c.cfg.auth != "" {
		add("proxy-authorization", c.cfg.auth)
	}
	add(headerPadding, paddingHeaderValue())
	if fastOpen {
		add("fastopen", "1")
	}
	for _, e := range c.cfg.extra {
		add(e.name, e.value)
	}
	add(headerPaddingTypeRequest, paddingTypeRequest)
	// Chromium sends no user agent on a tunnel naive opens; an empty value stops the default one.
	h["user-agent"] = []string{""}
	h[http.HeaderOrderKey] = order
	return h
}

// Close tears down every connection; the front has closed the tunnels already.
func (c *client) Close() error {
	c.closed.Store(true)
	for _, s := range c.slots {
		s.mu.Lock()
		if s.cc != nil {
			_ = s.cc.Close()
			s.cc = nil
		}
		s.mu.Unlock()
	}
	return nil
}

// errTunnel reports a CONNECT the server refused; the SOCKS reply code follows the status.
type errTunnel struct{ status int }

func (e errTunnel) Error() string {
	if e.status == http.StatusProxyAuthRequired {
		return "naive server rejected the credentials (407)"
	}
	return fmt.Sprintf("naive server answered CONNECT with %d", e.status)
}

func (e errTunnel) Unwrap() error {
	switch e.status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return sdk.ReplyError(0x04)
	case http.StatusForbidden:
		return sdk.ReplyError(0x02)
	}
	return sdk.ReplyError(0x01)
}

type tunnel struct {
	pw     *io.PipeWriter
	cancel context.CancelFunc
	ready  chan struct{}
	// Set before ready closes.
	err     error
	body    io.ReadCloser
	padding int

	writer io.Writer
	reader io.Reader
	wmu    sync.Mutex
	once   sync.Once
}

// responseStarted unblocks a refused CONNECT. fhttp's RoundTrip answers a non-2xx status only after the request body
// writer returns, and that writer sits in a read of the still-open tunnel pipe; a 200 makes RoundTrip return at
// once, so a tunnel still not ready a second after the first response byte was refused.
func (t *tunnel) responseStarted() {
	time.AfterFunc(refusalGrace, func() {
		select {
		case <-t.ready:
		default:
			_ = t.pw.CloseWithError(errors.New("CONNECT refused"))
		}
	})
}

func (t *tunnel) roundTrip(c *client, cc *http2.ClientConn, req *http.Request) {
	resp, err := cc.RoundTrip(req)
	switch {
	case err != nil:
		t.err = err
	case resp.StatusCode != http.StatusOK:
		_ = resp.Body.Close()
		t.err = errTunnel{resp.StatusCode}
		if resp.StatusCode == http.StatusProxyAuthRequired {
			c.status(sdk.StateDegraded, t.err.Error())
		}
	default:
		p, ok := replyPadding(resp.Header.Get)
		switch {
		case !ok:
			t.err = errors.New("naive server sent a bad padding-type-reply")
		case t.padding >= 0 && p != t.padding:
			t.err = errors.New("naive server changed the padding type of a fast-open tunnel")
		}
		if t.err != nil {
			_ = resp.Body.Close()
			break
		}
		c.padding.Store(int32(p))
		c.status(sdk.StateOK, "")
		t.padding, t.body = p, resp.Body
		t.reader = resp.Body
		if p == paddingVariant1 {
			t.reader = newPaddedReader(resp.Body)
		}
	}
	if t.err != nil {
		t.cancel()
		_ = t.pw.CloseWithError(t.err)
	}
	close(t.ready)
}

func (t *tunnel) newWriter(padding int) io.Writer {
	if padding == paddingVariant1 {
		return &paddedWriter{w: t.pw}
	}
	return t.pw
}

func (t *tunnel) Read(b []byte) (int, error) {
	<-t.ready
	if t.err != nil {
		return 0, t.err
	}
	return t.reader.Read(b)
}

func (t *tunnel) Write(b []byte) (int, error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	return t.writer.Write(b)
}

// CloseWrite ends the request stream, which the server passes on as a half-close.
func (t *tunnel) CloseWrite() error { return t.pw.Close() }

func (t *tunnel) Close() error {
	t.once.Do(func() {
		_ = t.pw.CloseWithError(net.ErrClosed)
		t.cancel()
		go func() {
			<-t.ready
			if t.body != nil {
				_ = t.body.Close()
			}
		}()
	})
	return nil
}

func (t *tunnel) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (t *tunnel) RemoteAddr() net.Addr { return &net.TCPAddr{} }

// Deadlines are not supported on an HTTP/2 stream; the front relays without them.
func (t *tunnel) SetDeadline(time.Time) error      { return nil }
func (t *tunnel) SetReadDeadline(time.Time) error  { return nil }
func (t *tunnel) SetWriteDeadline(time.Time) error { return nil }
