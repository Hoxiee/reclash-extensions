package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	olcclient "github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

// All values are fake: 203.0.113.0/24 is TEST-NET-3, and nothing below leaves the process.
const (
	fakeKey  = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	fakeRoom = "fake-room-0123456789"
)

func TestDecodeLink(t *testing.T) {
	tests := []struct {
		link string
		want config
	}{
		{
			"olcrtc://wbstream?vp8channel<vp8-fps=60&vp8-batch=32>@" + fakeRoom + "#" + fakeKey + "$RU / test node",
			config{
				Name: "RU / test node", Provider: "wbstream", Transport: "vp8channel", Room: fakeRoom, KeyHex: fakeKey,
				Server: "stream.wb.ru", Options: olcclient.VP8Options{FPS: 60, BatchSize: 32},
			},
		},
		{
			"OLCRTC://Telemost?vp8channel@" + fakeRoom + "#" + strings.ToUpper(fakeKey),
			config{
				Name: "olcrtc telemost", Provider: "telemost", Transport: "vp8channel", Room: fakeRoom, KeyHex: fakeKey,
				Server: "telemost.yandex.ru", Options: olcclient.VP8Options{FPS: 30, BatchSize: 64},
			},
		},
		{
			"olcrtc://telemost?seichannel<fps=60>@" + fakeRoom + "#" + fakeKey + "$DE%20node",
			config{
				Name: "DE node", Provider: "telemost", Transport: "seichannel", Room: fakeRoom, KeyHex: fakeKey,
				Server:  "telemost.yandex.ru",
				Options: olcclient.SEIOptions{FPS: 60, BatchSize: 64, FragmentSize: 900, AckTimeoutMS: 2000},
			},
		},
		{
			"olcrtc://jitsi?videochannel<video-codec=tile&video-tile-rs=20&video-bitrate=1M>@https://203.0.113.9/room#" +
				fakeKey,
			config{
				Name: "olcrtc jitsi", Provider: "jitsi", Transport: "videochannel", Room: "https://203.0.113.9/room",
				KeyHex: fakeKey, Server: "203.0.113.9",
				Options: olcclient.VideoOptions{
					Width: 1080, Height: 1080, FPS: 30, Codec: "tile", QRRecovery: "low", TileModule: 4, TileRS: 20,
				},
			},
		},
		{
			"olcrtc://jitsi?datachannel@meet.example.test/room#" + fakeKey + "$",
			config{
				Name: "olcrtc jitsi", Provider: "jitsi", Transport: "datachannel", Room: "meet.example.test/room",
				KeyHex: fakeKey, Server: "meet.example.test",
			},
		},
		{
			"olcrtc://wbstream?videochannel<video-w=1280&video-h=720&video-qr-recovery=high>@" + fakeRoom + "#" + fakeKey,
			config{
				Name: "olcrtc wbstream", Provider: "wbstream", Transport: "videochannel", Room: fakeRoom, KeyHex: fakeKey,
				Server:  "stream.wb.ru",
				Options: olcclient.VideoOptions{Width: 1280, Height: 720, FPS: 30, Codec: "qrcode", QRRecovery: "high"},
			},
		},
	}
	for _, tt := range tests {
		got, err := decodeLink(tt.link)
		if err != nil {
			t.Fatalf("%s: %v", tt.link, err)
		}
		if got != tt.want {
			t.Fatalf("%s:\n got %+v\nwant %+v", tt.link, got, tt.want)
		}
	}
}

func TestDecodeLinkRefuses(t *testing.T) {
	suffix := "@" + fakeRoom + "#" + fakeKey
	for _, link := range []string{
		"vless://x@203.0.113.1:443",
		"olcrtc://telemost",
		"olcrtc://telemost?vp8channel#" + fakeKey,
		"olcrtc://telemost?vp8channel@" + fakeRoom,
		"olcrtc://zoom?vp8channel" + suffix,
		"olcrtc://telemost?carrier-pigeon" + suffix,
		"olcrtc://telemost?" + suffix,
		"olcrtc://telemost?vp8channel@#" + fakeKey,
		"olcrtc://telemost?vp8channel@" + fakeRoom + "#abcd",
		"olcrtc://telemost?vp8channel@" + fakeRoom + "#" + strings.Repeat("zz", 32),
		"olcrtc://telemost?vp8channel<vp8-fps=60" + suffix,
		"olcrtc://telemost?vp8channel<vp8-fps=fast>" + suffix,
		"olcrtc://telemost?seichannel<ack-ms=-1>" + suffix,
		"olcrtc://telemost?videochannel<video-codec=png>" + suffix,
		"olcrtc://telemost?videochannel<video-qr-recovery=max>" + suffix,
		"olcrtc://telemost?videochannel<video-codec=tile&video-w=1920>" + suffix,
		"olcrtc://jitsi?vp8channel@meet.example.test#" + fakeKey,
	} {
		if c, err := decodeLink(link); err == nil {
			t.Fatalf("%s: accepted as %+v", link, c)
		}
	}
}

func TestDescribe(t *testing.T) {
	links := []string{
		"olcrtc://wbstream?vp8channel<vp8-fps=60&vp8-batch=64>@" + fakeRoom + "#" + fakeKey + "$Test node",
		"olcrtc://jitsi?datachannel@meet.example.test/" + fakeRoom + "#" + fakeKey,
		"olcrtc://telemost?vp8channel@" + fakeRoom + "#abcd",
	}
	req, _ := json.Marshal(sdk.DescribeRequest{Links: links})
	var out, errOut bytes.Buffer
	if code := sdk.Run(context.Background(), extension{}, []string{"describe"}, bytes.NewReader(req), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var resp sdk.DescribeResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 3 {
		t.Fatalf("nodes %+v", resp.Nodes)
	}
	if n := resp.Nodes[0]; n != (sdk.DescribedNode{Name: "Test node", Server: "stream.wb.ru"}) {
		t.Fatalf("node 0 %+v", n)
	}
	if n := resp.Nodes[1]; n != (sdk.DescribedNode{Name: "olcrtc jitsi", Server: "meet.example.test"}) {
		t.Fatalf("node 1 %+v", n)
	}
	if n := resp.Nodes[2]; n.Error == "" || n.Name != "" {
		t.Fatalf("node 2 %+v", n)
	}
	for _, secret := range []string{fakeKey, fakeRoom} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("describe leaks %q", secret)
		}
	}
}

func TestSecretsOf(t *testing.T) {
	c, err := decodeLink("olcrtc://jitsi?datachannel@https://meet.example.test/" + fakeRoom + "#" + fakeKey)
	if err != nil {
		t.Fatal(err)
	}
	l := sdk.NewLogger(io.Discard, sdk.LevelDebug)
	l.AddSecret(secretsOf(c)...)
	line := "joining https://meet.example.test/" + fakeRoom + " key=" + fakeKey + " KEY=" + strings.ToUpper(fakeKey) +
		" api=" + "https%3A%2F%2Fmeet.example.test%2F" + fakeRoom
	got := l.Redact(line)
	for _, secret := range []string{fakeKey, strings.ToUpper(fakeKey), fakeRoom} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted line %q still has %q", got, secret)
		}
	}
}

// refusingEgress is a fake Core egress that records each target and refuses it, so no test traffic leaves.
type refusingEgress struct {
	mu      sync.Mutex
	targets []string
	seen    chan struct{}
}

func newRefusingEgress() *refusingEgress { return &refusingEgress{seen: make(chan struct{}, 1)} }

func (e *refusingEgress) record(s string) {
	e.mu.Lock()
	e.targets = append(e.targets, s)
	e.mu.Unlock()
	select {
	case e.seen <- struct{}{}:
	default:
	}
}

func (e *refusingEgress) DialTCP(_ context.Context, address string) (net.Conn, error) {
	e.record("tcp " + address)
	return nil, sdk.ReplyError(0x05)
}

func (e *refusingEgress) DialUDP(_ context.Context, address string) (net.Conn, error) {
	e.record("udp " + address)
	return nil, sdk.ReplyError(0x05)
}

func (e *refusingEgress) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if strings.HasPrefix(network, "udp") {
		return e.DialUDP(ctx, address)
	}
	return e.DialTCP(ctx, address)
}

func (e *refusingEgress) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.targets)
}

func (e *refusingEgress) waitFor(t *testing.T, what string, ok func(string) bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !slices.ContainsFunc(e.list(), ok) {
		select {
		case <-e.seen:
		case <-deadline:
			t.Fatalf("egress saw %v, want %s", e.list(), what)
		}
	}
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

// A running client asks the provider for room credentials through the egress, by host name, reports degraded
// while it cannot, and stops on stdin EOF. Each case runs in its own process state, so it runs one link.
func TestRunDialsThroughEgress(t *testing.T) {
	eg := newRefusingEgress()
	front, err := sdk.NewFront("127.0.0.1:0", eg)
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()

	link := "olcrtc://telemost?vp8channel<vp8-fps=60&vp8-batch=64>@" + fakeRoom + "#" + fakeKey + "$Test node"
	req, _ := json.Marshal(sdk.RunRequest{
		Protocol: 1, Link: link, Listen: "127.0.0.1:0", DataDir: t.TempDir(), LogLevel: "debug",
		Egress: sdk.EgressConfig{SOCKS5: front.Addr(), Username: front.Username(), Password: front.Password()},
	})
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &syncBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- sdk.Run(context.Background(), extension{}, []string{"run"}, stdinR, stdoutW, stderr)
		_ = stdoutW.Close()
	}()
	go func() { _, _ = stdinW.Write(req) }()

	events := make(chan sdk.Event, 16)
	go func() {
		s := bufio.NewScanner(stdoutR)
		for s.Scan() {
			var ev sdk.Event
			if json.Unmarshal(s.Bytes(), &ev) == nil {
				events <- ev
			}
		}
		close(events)
	}()
	select {
	case ev := <-events:
		if ev.Event != sdk.EventReady || ev.UDP {
			t.Fatalf("first event %+v; stderr:\n%s", ev, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no ready event; stderr:\n%s", stderr.String())
	}
	select {
	case ev := <-events:
		if ev.Event != sdk.EventStatus || ev.State != sdk.StateDegraded {
			t.Fatalf("second event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no status event; stderr:\n%s", stderr.String())
	}

	eg.waitFor(t, "the Telemost API by name on :443", func(s string) bool {
		return s == "tcp cloud-api.yandex.ru:443"
	})
	for _, s := range eg.list() {
		addr := strings.TrimPrefix(strings.TrimPrefix(s, "tcp "), "udp ")
		if host, _, _ := net.SplitHostPort(addr); net.ParseIP(host) != nil && host != "77.88.8.8" {
			t.Fatalf("egress dialed a resolved address %q", s)
		}
	}

	t.Logf("egress saw %v", eg.list())
	_ = stdinW.Close()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit %d; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("still running 5 s after stdin EOF")
	}

	log := stderr.String()
	for _, secret := range []string{fakeKey, fakeRoom, front.Password()} {
		if strings.Contains(log, secret) {
			t.Fatalf("stderr leaks %q", secret)
		}
	}
}

func TestResolverUsesEgress(t *testing.T) {
	eg := newRefusingEgress()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := egressResolver(eg).LookupNetIP(ctx, "ip4", "vpn.example.test"); err == nil {
		t.Fatal("lookup through a refusing egress succeeded")
	}
	if got := eg.list(); len(got) == 0 || got[0] != "tcp 77.88.8.8:443" {
		t.Fatalf("egress saw %v", got)
	}
}

// The resolver's DoH framing round-trips one query to a local DoH server.
func TestDoHConnFraming(t *testing.T) {
	query := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	answer := []byte{0x12, 0x34, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
	var got []byte
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		got, _ = io.ReadAll(r.Body)
		if r.URL.String() != dohURL || r.Header.Get("Content-Type") != "application/dns-message" {
			t.Errorf("request %s %v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(answer)), Request: r}, nil
	})}
	c := newDoHConn(client)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	framed := append([]byte{0, byte(len(query))}, query...)
	if _, err := c.Write(framed[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(framed[3:]); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := io.ReadAtLeast(c, buf, 2+len(answer))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, query) || !bytes.Equal(buf[:n], append([]byte{0, byte(len(answer))}, answer...)) {
		t.Fatalf("sent %x, read %x", got, buf[:n])
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
