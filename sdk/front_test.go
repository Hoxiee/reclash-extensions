package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func tcpEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func udpEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().String()
}

// directProxy is the fake Core egress: it dials the host network and records every target it was asked for.
type directProxy struct {
	mu      sync.Mutex
	targets []string
}

func (p *directProxy) record(s string) {
	p.mu.Lock()
	p.targets = append(p.targets, s)
	p.mu.Unlock()
}

func (p *directProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.targets)
}

func (p *directProxy) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	p.record("tcp " + address)
	var d net.Dialer
	return d.DialContext(ctx, "tcp", address)
}

func (p *directProxy) DialUDP(ctx context.Context, address string) (net.Conn, error) {
	p.record("udp " + address)
	var d net.Dialer
	return d.DialContext(ctx, "udp", address)
}

func fakeEgress(t *testing.T) (EgressConfig, *directProxy) {
	t.Helper()
	p := &directProxy{}
	f, err := NewFront("127.0.0.1:0", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return EgressConfig{SOCKS5: f.Addr(), Username: f.Username(), Password: f.Password()}, p
}

func ctxTimeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func echoOnce(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != msg {
		t.Fatalf("echo %q, want %q", got, msg)
	}
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The front speaks RFC 1928/1929 byte for byte, independent of the SDK's own client.
func TestFrontWireFormat(t *testing.T) {
	echo := tcpEcho(t)
	p := &directProxy{}
	f, err := NewFront("127.0.0.1:0", p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", f.Addr())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	auth := func(c net.Conn, user, pass string) []byte {
		_, _ = c.Write([]byte{5, 2, 0, 2})
		if got := readN(t, c, 2); !bytes.Equal(got, []byte{5, 2}) {
			t.Fatalf("method reply %v", got)
		}
		b := append([]byte{1, byte(len(user))}, user...)
		b = append(append(b, byte(len(pass))), pass...)
		_, _ = c.Write(b)
		return readN(t, c, 2)
	}

	c := dial()
	_, _ = c.Write([]byte{5, 1, 0})
	if got := readN(t, c, 2); !bytes.Equal(got, []byte{5, 0xff}) {
		t.Fatalf("no-auth greeting answered %v", got)
	}
	if got := auth(dial(), f.Username(), "wrong"); !bytes.Equal(got, []byte{1, 1}) {
		t.Fatalf("wrong password answered %v", got)
	}

	c = dial()
	if got := auth(c, f.Username(), f.Password()); !bytes.Equal(got, []byte{1, 0}) {
		t.Fatalf("auth answered %v", got)
	}
	host, port, _ := net.SplitHostPort(echo)
	pn, _ := strconv.Atoi(port)
	req := append([]byte{5, 1, 0, 1}, net.ParseIP(host).To4()...)
	_, _ = c.Write(binary.BigEndian.AppendUint16(req, uint16(pn)))
	reply := readN(t, c, 10)
	if reply[0] != 5 || reply[1] != 0 || reply[3] != 1 {
		t.Fatalf("connect reply %v", reply)
	}
	echoOnce(t, c, "ping")

	c = dial()
	auth(c, f.Username(), f.Password())
	_, _ = c.Write([]byte{5, 2, 0, 1, 127, 0, 0, 1, 0, 80})
	if reply := readN(t, c, 10); reply[1] != repCommandNotSupported {
		t.Fatalf("bind reply %v", reply)
	}

	if got := p.seen(); !slices.Equal(got, []string{"tcp " + echo}) {
		t.Fatalf("targets %v", got)
	}
}

func TestEgressTCPAndUDP(t *testing.T) {
	cfg, p := fakeEgress(t)
	e := NewEgress(cfg)
	ctx := ctxTimeout(t)

	echo := tcpEcho(t)
	c, err := e.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	echoOnce(t, c, "hello tcp")
	_ = c.Close()

	// Names reach the egress unresolved.
	_, port, _ := net.SplitHostPort(echo)
	c, err = e.DialContext(ctx, "tcp", "localhost:"+port)
	if err != nil {
		t.Fatal(err)
	}
	echoOnce(t, c, "by name")
	_ = c.Close()

	uecho := udpEcho(t)
	u, err := e.DialContext(ctx, "udp", uecho)
	if err != nil {
		t.Fatal(err)
	}
	echoOnce(t, u, "hello udp")
	echoOnce(t, u, "again")
	_ = u.Close()

	pc, err := e.ListenPacket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_ = pc.SetDeadline(time.Now().Add(5 * time.Second))
	dst, _ := net.ResolveUDPAddr("udp", uecho)
	if _, err := pc.WriteTo([]byte("packet"), dst); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	n, from, err := pc.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "packet" || from.String() != uecho {
		t.Fatalf("ReadFrom %q from %v: %v", buf[:n], from, err)
	}

	want := []string{"tcp " + echo, "tcp localhost:" + port, "udp " + uecho, "udp " + uecho}
	if got := p.seen(); !slices.Equal(got, want) {
		t.Fatalf("egress saw %v, want %v", got, want)
	}

	bad := NewEgress(EgressConfig{SOCKS5: cfg.SOCKS5, Username: cfg.Username, Password: "nope"})
	if _, err := bad.DialContext(ctx, "tcp", echo); err == nil {
		t.Fatal("wrong egress password accepted")
	}
}

func TestFrontLoopbackOnly(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:0", ":0", "localhost:0", "192.0.2.1:0"} {
		if f, err := NewFront(listen, &directProxy{}); err == nil {
			_ = f.Close()
			t.Fatalf("NewFront(%q) accepted a non-loopback address", listen)
		}
	}
}

// egressExt carries every front request to the egress, logging the secrets it must never print.
type egressExt struct{ rt *Runtime }

func (*egressExt) Info() Info { return Info{ID: "test", Version: "0.0.1"} }

func (*egressExt) Describe(string) (Node, error) { return Node{}, nil }

func (x *egressExt) Start(_ context.Context, rt *Runtime) (Proxy, error) {
	x.rt = rt
	rt.Log.AddSecret("decoded-secret-value")
	rt.Log.Infof("starting %s with decoded-secret-value", rt.Link)
	rt.Log.Debugf("debug lines are off at info")
	rt.Status(StateDegraded, "connecting")
	return egressProxy{rt}, nil
}

type egressProxy struct{ rt *Runtime }

func (p egressProxy) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	p.rt.Log.Infof("dial %s for %s", address, p.rt.Link)
	return p.rt.Egress.DialContext(ctx, "tcp", address)
}

func (p egressProxy) DialUDP(ctx context.Context, address string) (net.Conn, error) {
	return p.rt.Egress.DialContext(ctx, "udp", address)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunEndToEnd(t *testing.T) {
	cfg, p := fakeEgress(t)
	echo, uecho := tcpEcho(t), udpEcho(t)
	link := "test://v1/c2VjcmV0LWxpbmstYm9keQ"
	req := RunRequest{Protocol: 1, Link: link, Listen: "127.0.0.1:0", Egress: cfg, DataDir: t.TempDir(), LogLevel: "info"}
	raw, _ := json.Marshal(req)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &syncBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- Run(context.Background(), &egressExt{}, []string{"run"}, stdinR, stdoutW, stderr)
		_ = stdoutW.Close()
	}()
	go func() { _, _ = stdinW.Write(raw) }()

	lines := bufio.NewScanner(stdoutR)
	next := func() Event {
		if !lines.Scan() {
			t.Fatalf("stdout ended: %v; stderr:\n%s", lines.Err(), stderr.String())
		}
		var ev Event
		if err := json.Unmarshal(lines.Bytes(), &ev); err != nil {
			t.Fatalf("bad event %q: %v", lines.Text(), err)
		}
		return ev
	}
	ready := next()
	if ready.Event != EventReady || !ready.UDP || ready.Username == "" || ready.Password == "" {
		t.Fatalf("ready: %+v", ready)
	}
	if host, port, _ := net.SplitHostPort(ready.SOCKS5); host != "127.0.0.1" || port == "0" {
		t.Fatalf("front address %q", ready.SOCKS5)
	}
	if st := next(); st.Event != EventStatus || st.State != StateDegraded || st.Message != "connecting" {
		t.Fatalf("status after ready: %+v", st)
	}
	go func() {
		for lines.Scan() {
		}
	}()

	core := NewEgress(EgressConfig{SOCKS5: ready.SOCKS5, Username: ready.Username, Password: ready.Password})
	ctx := ctxTimeout(t)
	c, err := core.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	echoOnce(t, c, "through the extension")
	u, err := core.DialContext(ctx, "udp", uecho)
	if err != nil {
		t.Fatal(err)
	}
	echoOnce(t, u, "udp through the extension")
	_ = u.Close()

	if got := p.seen(); !slices.Contains(got, "tcp "+echo) || !slices.Contains(got, "udp "+uecho) {
		t.Fatalf("egress saw %v", got)
	}

	start := time.Now()
	_ = stdinW.Close()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("extension did not stop within 3 s of stdin EOF")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("stop took %s", d)
	}
	_ = c.Close()

	log := stderr.String()
	for _, secret := range []string{link, "c2VjcmV0LWxpbmstYm9keQ", cfg.Password, ready.Password, "decoded-secret-value"} {
		if strings.Contains(log, secret) {
			t.Fatalf("stderr leaks %q:\n%s", secret, log)
		}
	}
	if !strings.Contains(log, "[redacted]") || strings.Contains(log, "debug lines") {
		t.Fatalf("unexpected log:\n%s", log)
	}
}

func TestLoggerRedactsAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	l := NewLogger(&out, LevelInfo)
	l.AddSecret("s3cr3t/with space", "abc")
	w := l.Writer(LevelInfo)
	_, _ = w.Write([]byte("one s3cr"))
	_, _ = w.Write([]byte("3t/with space two\nescaped s3cr3t%2Fwith+space and s3cr3t%2Fwith%20space\n"))
	_, _ = l.Writer(LevelDebug).Write([]byte("hidden s3cr3t/with space\n"))
	got := out.String()
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "hidden") {
		t.Fatalf("leak:\n%s", got)
	}
	if strings.Count(got, "[redacted]") != 3 || strings.Count(got, "\n") != 2 {
		t.Fatalf("unexpected:\n%s", got)
	}
	// Strings under minSecret characters are not redacted.
	if !strings.Contains(l.Redact("abc"), "abc") {
		t.Fatal("short secret redacted")
	}
}

func TestStatusDeduplicated(t *testing.T) {
	var out bytes.Buffer
	e := &eventWriter{w: &out}
	e.status(StateDegraded, "a")
	e.status(StateOK, "")
	e.ready(Event{Event: EventReady})
	e.status(StateOK, "")
	e.status(StateDegraded, "b")
	e.status(StateDegraded, "b")
	e.fatal("x")
	e.status(StateOK, "")
	want := `{"event":"ready"}` + "\n" + `{"event":"status","state":"ok"}` + "\n" +
		`{"event":"status","state":"degraded","message":"b"}` + "\n" + `{"event":"fatal","message":"x"}` + "\n"
	if out.String() != want {
		t.Fatalf("events:\n%s\nwant:\n%s", out.String(), want)
	}
}
