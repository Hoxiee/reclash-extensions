package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	"golang.zx2c4.com/wireguard/conn"
)

// All values are fake: 203.0.113.0/24 is TEST-NET-3, and nothing below leaves the process.
const (
	fakePass  = "fake-pass-0123456789"
	fakeHash1 = "fakehash0000000000000000001"
	fakeHash2 = "fakehash0000000000000000002"
	fakePeer  = "203.0.113.7:56000"
)

func makeLink(params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return "qwdtt://config?" + q.Encode()
}

func baseParams() map[string]string {
	return map[string]string{
		"name": "Test node", "peer": fakePeer, "hashes": fakeHash1 + "," + fakeHash2, "workers": "16",
		"port": "9100", "pass": fakePass,
	}
}

func with(p map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range p {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(out, kv[i])
		} else {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

func TestDecodeLink(t *testing.T) {
	c, err := decodeLink(makeLink(baseParams()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Test node" || c.Peer != fakePeer || c.Pass != fakePass || c.Workers != 16 || c.Port != "9100" ||
		!slices.Equal(c.Hashes, []string{fakeHash1, fakeHash2}) {
		t.Fatalf("decoded %+v", c)
	}

	opaque := strings.Replace(makeLink(with(baseParams(), "pass", "", "password", fakePass, "workers", "", "port", "")),
		"qwdtt://", "qwdtt:", 1)
	c, err = decodeLink(opaque)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pass != fakePass || c.Workers != defaultWorkers || c.Port != "9000" {
		t.Fatalf("opaque form %+v", c)
	}
}

func TestDecodeLinkRefuses(t *testing.T) {
	for name, link := range map[string]string{
		"other scheme": "vless://x",
		"other host":   strings.Replace(makeLink(baseParams()), "//config", "//node", 1),
		"no pass":      makeLink(with(baseParams(), "pass", "")),
		"no hashes":    makeLink(with(baseParams(), "hashes", "")),
		"no peer port": makeLink(with(baseParams(), "peer", "203.0.113.7")),
		"bad port":     makeLink(with(baseParams(), "port", "70000")),
		"bad workers":  makeLink(with(baseParams(), "workers", "0")),
		"raw mode":     makeLink(with(baseParams(), "mode", "raw")),
	} {
		if _, err := decodeLink(link); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDescribe(t *testing.T) {
	links := []string{
		makeLink(baseParams()),
		makeLink(with(baseParams(), "name", "", "peer", "vpn.example.test:56000")),
		makeLink(with(baseParams(), "pass", "")),
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
	if n := resp.Nodes[0]; n != (sdk.DescribedNode{Name: "Test node", Server: "203.0.113.7", UDP: true}) {
		t.Fatalf("node 0 %+v", n)
	}
	if n := resp.Nodes[1]; n != (sdk.DescribedNode{Server: "vpn.example.test", UDP: true}) {
		t.Fatalf("node 1 %+v", n)
	}
	if n := resp.Nodes[2]; n.Error == "" || n.Name != "" {
		t.Fatalf("node 2 %+v", n)
	}
	for _, secret := range []string{fakePass, fakeHash1, fakeHash2} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("describe leaks %q", secret)
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
	if network == "udp" {
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

// A running client asks VK for TURN credentials through the egress, by host name, and stops on stdin EOF.
func TestRunDialsThroughEgress(t *testing.T) {
	eg := newRefusingEgress()
	front, err := sdk.NewFront("127.0.0.1:0", eg)
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()

	link := makeLink(with(baseParams(), "workers", "9"))
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
		if ev.Event != sdk.EventReady || !ev.UDP {
			t.Fatalf("first event %+v; stderr:\n%s", ev, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no ready event; stderr:\n%s", stderr.String())
	}

	eg.waitFor(t, "a VK host by name on :443", func(s string) bool {
		return strings.HasPrefix(s, "tcp ") && strings.Contains(s, "vk") && strings.HasSuffix(s, ":443")
	})
	for _, s := range eg.list() {
		if host, _, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(s, "tcp "), "udp ")); net.ParseIP(host) != nil &&
			host != "77.88.8.8" {
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
	for _, secret := range []string{fakePass, fakeHash1, fakeHash2, front.Password()} {
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

func TestTURNDialUsesEgress(t *testing.T) {
	eg := newRefusingEgress()
	useEgress(context.Background(), eg)
	for _, tcp := range []bool{false, true} {
		if _, _, err := dialTURNConn("turn.example.test:3478", tcp); err == nil {
			t.Fatal("dial through a refusing egress succeeded")
		}
	}
	if got := eg.list(); !slices.Equal(got, []string{"udp turn.example.test:3478", "tcp turn.example.test:3478"}) {
		t.Fatalf("egress saw %v", got)
	}
}

func TestWGPipe(t *testing.T) {
	p := newWGPipe()
	b := p.bind()
	fns, _, err := b.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	pc := p.packetConn()

	if err := b.Send([][]byte{[]byte("up-1"), []byte("up-2")}, pipeEndpoint{}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	for _, want := range []string{"up-1", "up-2"} {
		n, _, err := pc.ReadFrom(buf)
		if err != nil || string(buf[:n]) != want {
			t.Fatalf("dispatcher read %q, %v; want %q", buf[:n], err, want)
		}
	}

	for _, s := range []string{"down-1", "down-2"} {
		if _, err := pc.WriteTo([]byte(s), nil); err != nil {
			t.Fatal(err)
		}
	}
	packets := [][]byte{make([]byte, 64), make([]byte, 64), make([]byte, 64)}
	sizes := make([]int, 3)
	eps := make([]conn.Endpoint, 3)
	n, err := fns[0](packets, sizes, eps)
	if err != nil || n != 2 || string(packets[0][:sizes[0]]) != "down-1" || string(packets[1][:sizes[1]]) != "down-2" {
		t.Fatalf("device received %d, %v", n, err)
	}

	_ = b.Close()
	if _, err := fns[0](packets, sizes, eps); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("receive after Bind.Close: %v", err)
	}
	if _, _, err := b.Open(0); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = p.Close()
	if _, _, err := pc.ReadFrom(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after Close: %v", err)
	}
}

func TestDeviceID(t *testing.T) {
	dir := t.TempDir()
	a, b := deviceIDFor(fakePass, dir), deviceIDFor(fakePass, dir)
	if len(a) != 16 || a != b {
		t.Fatalf("device ids %q, %q", a, b)
	}
	if deviceIDFor(fakePass+"x", dir) == a {
		t.Fatal("device id does not depend on the password")
	}
}
