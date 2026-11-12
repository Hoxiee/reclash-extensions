package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
)

// All values are fake: 203.0.113.0/24 is TEST-NET-3 and nothing below is dialed except a loopback test server.
const fakeLink = "naive+https://u:p@203.0.113.7:8443#Test%20node"

func TestDecodeLink(t *testing.T) {
	c, err := decodeLink(fakeLink)
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "203.0.113.7" || c.port != "8443" || c.name != "Test node" || c.username != "u" || c.password != "p" {
		t.Fatalf("decoded %+v", c)
	}
	if c.auth != "Basic dTpw" || c.conns != 1 || c.server() != "203.0.113.7:8443" {
		t.Fatalf("auth %q conns %d server %q", c.auth, c.conns, c.server())
	}

	c, err = decodeLink("naive+https://u:p@example.test?insecure-concurrency=99&extra-headers=X-A%3A%20b%0D%0AX-C%3A%20d&padding=true")
	if err != nil {
		t.Fatal(err)
	}
	if c.port != "443" || c.name != "example.test" || c.conns != maxConcurrency {
		t.Fatalf("defaults %+v", c)
	}
	if !slices.Equal(c.extra, []header{{"x-a", "b"}, {"x-c", "d"}}) {
		t.Fatalf("extra %+v", c.extra)
	}

	c, err = decodeLink("naive+https://203.0.113.7")
	if err != nil || c.auth != "" {
		t.Fatalf("no credentials: %+v %v", c, err)
	}
}

func TestDecodeLinkRefuses(t *testing.T) {
	for name, link := range map[string]string{
		"not a link":     "::",
		"unknown scheme": "naive+tcp://u:p@203.0.113.7:443",
		"plain https":    "https://u:p@203.0.113.7:443",
		"quic":           "naive+quic://u:p@203.0.113.7:443",
		"no host":        "naive+https://u:p@:443",
		"bad port":       "naive+https://u:p@203.0.113.7:70000",
		"zero port":      "naive+https://u:p@203.0.113.7:0",
		"path":           "naive+https://u:p@203.0.113.7:443/x",
		"bad padding":    "naive+https://u:p@203.0.113.7:443?padding=maybe",
		"bad conc":       "naive+https://u:p@203.0.113.7:443?insecure-concurrency=0",
		"reserved hdr":   "naive+https://u:p@203.0.113.7:443?extra-headers=Padding%3A%20x",
		"bad hdr":        "naive+https://u:p@203.0.113.7:443?extra-headers=nocolon",
	} {
		if _, err := decodeLink(link); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := decodeLink("naive+quic://u:p@203.0.113.7:443"); err == nil || !strings.Contains(err.Error(), "quic") {
		t.Errorf("quic error %v", err)
	}
}

func TestDescribe(t *testing.T) {
	n, err := describeLink(fakeLink)
	if err != nil {
		t.Fatal(err)
	}
	if n != (sdk.Node{Name: "Test node", Server: "203.0.113.7", UDP: false}) {
		t.Fatalf("node %+v", n)
	}
	if _, err := describeLink("naive+quic://u:p@203.0.113.7:443"); err == nil {
		t.Fatal("quic described")
	}
}

func TestSecrets(t *testing.T) {
	c, err := decodeLink("naive+https://u:p@203.0.113.7:8443?extra-headers=X-Key%3A%20hdrsecret")
	if err != nil {
		t.Fatal(err)
	}
	got := secretsOf(c)
	for _, want := range []string{"u", "p", "Basic dTpw", "dTpw", "hdrsecret"} {
		if !slices.Contains(got, want) {
			t.Errorf("secret %q missing from %q", want, got)
		}
	}
}

func TestPaddedRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	w := &paddedWriter{w: &wire}
	var want bytes.Buffer
	// More writes than firstPaddings, one larger than a frame holds, so the framed and the bare part both occur.
	for i, size := range []int{1, 5, 300, 70000, 2, 9, 1000, 4, 33, 17, 200000} {
		chunk := bytes.Repeat([]byte{byte('a' + i)}, size)
		want.Write(chunk)
		if n, err := w.Write(chunk); err != nil || n != size {
			t.Fatalf("write %d: %d %v", i, n, err)
		}
	}
	if w.frames != firstPaddings {
		t.Fatalf("frames %d", w.frames)
	}
	got, err := io.ReadAll(newPaddedReader(&wire))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("round trip differs: %d bytes, want %d", len(got), want.Len())
	}
}

func TestPaddedFrameFormat(t *testing.T) {
	var wire bytes.Buffer
	w := &paddedWriter{w: &wire}
	payload := bytes.Repeat([]byte("x"), 0x0123)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	b := wire.Bytes()
	pad := int(b[2])
	if b[0] != 0x01 || b[1] != 0x23 || len(b) != frameHeader+len(payload)+pad {
		t.Fatalf("header % x, %d bytes, padding %d", b[:3], len(b), pad)
	}
	if !bytes.Equal(b[3:3+len(payload)], payload) || !bytes.Equal(b[3+len(payload):], make([]byte, pad)) {
		t.Fatal("payload or zero padding wrong")
	}
}

func TestPaddedReaderTruncated(t *testing.T) {
	frame := append([]byte{0, 4, 2}, "abcd\x00"...)
	_, err := io.ReadAll(newPaddedReader(bytes.NewReader(frame)))
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("err %v", err)
	}
}

func TestReplyPadding(t *testing.T) {
	hdr := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		name string
		h    map[string]string
		want int
		ok   bool
	}{
		{"variant1", map[string]string{headerPaddingTypeReply: "1"}, paddingVariant1, true},
		{"none", map[string]string{headerPaddingTypeReply: "0"}, paddingNone, true},
		{"unknown", map[string]string{headerPaddingTypeReply: "2"}, 0, false},
		{"garbage", map[string]string{headerPaddingTypeReply: "x"}, 0, false},
		{"legacy", map[string]string{headerPadding: "zzz"}, paddingVariant1, true},
		{"plain", nil, paddingNone, true},
	} {
		got, ok := replyPadding(hdr(tc.h))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %d %v", tc.name, got, ok)
		}
	}
}

func TestPaddingHeaderValue(t *testing.T) {
	for range 50 {
		v := paddingHeaderValue()
		if len(v) < 16 || len(v) > 32 {
			t.Fatalf("length %d", len(v))
		}
		for _, c := range []byte(v) {
			if c < 0x20 || c > 0x7e {
				t.Fatalf("byte %#x", c)
			}
		}
	}
}

func TestRequestHeader(t *testing.T) {
	c := newClient(config{auth: "Basic dTpw", extra: []header{{"x-a", "b"}}, conns: 1}, nil, nil)
	h := c.requestHeader(false)
	if h["proxy-authorization"][0] != "Basic dTpw" || h[headerPaddingTypeRequest][0] != "1, 0" || h["fastopen"] != nil ||
		h[headerPadding] == nil || h["x-a"][0] != "b" {
		t.Fatalf("header %v", h)
	}
	if c.requestHeader(true)["fastopen"][0] != "1" {
		t.Fatal("no fastopen")
	}
}

type countingDialer struct {
	addr  string
	calls atomic.Int32
	asked atomic.Value
}

func (d *countingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.calls.Add(1)
	d.asked.Store(network + " " + address)
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func testServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "naive.test"}, DNSNames: []string{"naive.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"h2"}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return srv, pool
}

// echoHandler is a naive server: it checks the credentials, answers the padding negotiation and echoes the tunnel.
func echoHandler(padReply string, gotHeaders chan<- http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != "Basic dTpw" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		select {
		case gotHeaders <- r.Header.Clone():
		default:
		}
		if padReply != "" {
			w.Header().Set(headerPaddingTypeReply, padReply)
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		var in io.Reader = r.Body
		var out io.Writer = flushWriter{w}
		if padReply == "1" {
			in = newPaddedReader(r.Body)
			out = &paddedWriter{w: out}
		}
		_, _ = io.Copy(out, in)
	}
}

type flushWriter struct{ http.ResponseWriter }

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.ResponseWriter.Write(b)
	f.ResponseWriter.(http.Flusher).Flush()
	return n, err
}

func TestTunnelThroughEgress(t *testing.T) {
	for _, padReply := range []string{"1", "0", ""} {
		headers := make(chan http.Header, 4)
		srv, pool := testServer(t, echoHandler(padReply, headers))
		old := rootCAs
		rootCAs = pool
		t.Cleanup(func() { rootCAs = old })

		d := &countingDialer{addr: srv.Listener.Addr().String()}
		c := newClient(config{host: "naive.test", port: "443", auth: "Basic dTpw", conns: 1}, d, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		for round := range 2 {
			conn, err := c.DialTCP(ctx, "target.test:80")
			if err != nil {
				t.Fatalf("pad %q round %d: %v", padReply, round, err)
			}
			msg := bytes.Repeat([]byte("hello"), 5000)
			for i := range 12 {
				if _, err := conn.Write(msg[:100*(i+1)]); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, 100*(i+1))
				if _, err := io.ReadFull(conn, got); err != nil {
					t.Fatalf("pad %q round %d read %d: %v", padReply, round, i, err)
				}
				if !bytes.Equal(got, msg[:len(got)]) {
					t.Fatalf("pad %q: echo differs", padReply)
				}
			}
			_ = conn.Close()
		}
		if d.calls.Load() != 1 || d.asked.Load() != "tcp naive.test:443" {
			t.Fatalf("dials %d, asked %v", d.calls.Load(), d.asked.Load())
		}
		h := <-headers
		if h.Get(headerPaddingTypeRequest) != "1, 0" || h.Get(headerPadding) == "" {
			t.Fatalf("padding headers %v", h)
		}
		if second := <-headers; second.Get("Fastopen") != "1" {
			t.Fatalf("second tunnel without fastopen, reply %q", padReply)
		}
		_ = c.Close()
		cancel()
	}
}

func TestTunnelRefused(t *testing.T) {
	srv, pool := testServer(t, echoHandler("1", nil))
	old := rootCAs
	rootCAs = pool
	t.Cleanup(func() { rootCAs = old })
	d := &countingDialer{addr: srv.Listener.Addr().String()}
	c := newClient(config{host: "naive.test", port: "443", auth: "Basic wrong", conns: 1}, d, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.DialTCP(ctx, "target.test:80")
	var te errTunnel
	if !asTunnelErr(err, &te) || te.status != http.StatusProxyAuthRequired {
		t.Fatalf("err %v", err)
	}
}

func asTunnelErr(err error, out *errTunnel) bool {
	e, ok := err.(errTunnel)
	if ok {
		*out = e
	}
	return ok
}
