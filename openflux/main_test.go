package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
)

// All values are fake: 203.0.113.0/24 is TEST-NET-3 and nothing below is ever dialed.
const (
	fakeSecret = "fake-secret-0123456789abcdef"
	fakeDoc    = "https://disk.yandex.ru/d/fake-document-0001"
	fakeDial   = "203.0.113.7:8445"
)

func linkJSON(transports string) string {
	return `{"name":"Test node","negotiate":true,"secret":"` + fakeSecret + `","context":"` + fakeDoc +
		`","transports":[` + transports + `]}`
}

const bothCarriers = `{"type":"direct","priority":100,"dial":"` + fakeDial + `"},` +
	`{"type":"yandex","url":"` + fakeDoc + `","priority":50}`

func makeLink(t *testing.T, payload string, enc *base64.Encoding) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(payload))
	_ = w.Close()
	return "openflux://v1/" + enc.EncodeToString(buf.Bytes())
}

func TestDecodeLink(t *testing.T) {
	payload := linkJSON(bothCarriers)
	// Padding and the standard alphabet are accepted like the canonical form.
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding} {
		c, err := decodeLink(makeLink(t, payload, enc))
		if err != nil {
			t.Fatal(err)
		}
		if c.Name != "Test node" || !c.Negotiate || c.Secret != fakeSecret || c.Context != fakeDoc || len(c.Transports) != 2 {
			t.Fatalf("decoded %+v", c)
		}
		if d := c.Transports[0]; d.Type != "direct" || d.Dial != fakeDial || d.Priority != 100 {
			t.Fatalf("direct %+v", d)
		}
		if y := c.Transports[1]; y.Type != "yandex" || y.URL != fakeDoc || y.Priority != 50 {
			t.Fatalf("yandex %+v", y)
		}
	}
}

func TestDecodeLinkRefuses(t *testing.T) {
	for name, link := range map[string]string{
		"not a link":  "vless://x",
		"broken":      "openflux://v1/broken",
		"version":     "openflux://v2/abc",
		"telemost":    makeLink(t, linkJSON(`{"type":"telemost","url":"https://telemost.yandex.ru/j/1"}`), base64.RawURLEncoding),
		"no secret":   makeLink(t, `{"transports":[{"type":"yandex","url":"`+fakeDoc+`"}]}`, base64.RawURLEncoding),
		"stream mode": makeLink(t, `{"mode":"stream","transports":[{"type":"mailru","url":"https://cloud.mail.ru/x"}]}`, base64.RawURLEncoding),
	} {
		if _, err := decodeLink(link); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDescribe(t *testing.T) {
	links := []string{
		makeLink(t, linkJSON(bothCarriers), base64.RawURLEncoding),
		makeLink(t, linkJSON(`{"type":"yandex","url":"`+fakeDoc+`"}`), base64.RawURLEncoding),
		"openflux://v1/broken",
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
	if n := resp.Nodes[1]; n.Name != "Test node" || n.Server != "" || n.Error != "" {
		t.Fatalf("node 1 %+v", n)
	}
	if n := resp.Nodes[2]; n.Error == "" || n.Name != "" {
		t.Fatalf("node 2 %+v", n)
	}
	if strings.Contains(out.String(), fakeSecret) {
		t.Fatal("describe leaks the secret")
	}
}

// refusingEgress is a fake Core egress that records each target and refuses it, so no test traffic leaves.
type refusingEgress struct {
	mu      sync.Mutex
	targets []string
	seen    chan struct{}
}

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

func (e *refusingEgress) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.targets)
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

// The carriers of a running client dial only through the egress: direct by the link's address, the Yandex
// carrier by host name.
func TestRunDialsThroughEgress(t *testing.T) {
	eg := &refusingEgress{seen: make(chan struct{}, 1)}
	front, err := sdk.NewFront("127.0.0.1:0", eg)
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()

	link := makeLink(t, linkJSON(bothCarriers), base64.RawURLEncoding)
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

	wantDirect := "tcp " + fakeDial
	deadline := time.After(10 * time.Second)
	for {
		got := eg.list()
		yandex := slices.ContainsFunc(got, func(s string) bool {
			return strings.HasPrefix(s, "tcp ") && strings.Contains(s, "yandex") && strings.HasSuffix(s, ":443")
		})
		if slices.Contains(got, wantDirect) && yandex {
			break
		}
		select {
		case <-eg.seen:
		case <-deadline:
			t.Fatalf("egress saw %v, want %q and a Yandex host by name; stderr:\n%s", got, wantDirect, stderr.String())
		}
	}

	start := time.Now()
	_ = stdinW.Close()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit %d; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("still running 3 s after stdin EOF")
	}
	t.Logf("stopped in %s; egress saw %v", time.Since(start).Round(time.Millisecond), eg.list())

	log := stderr.String()
	for _, secret := range []string{link, fakeSecret, fakeDoc, front.Password()} {
		if strings.Contains(log, secret) {
			t.Fatalf("stderr leaks %q", secret)
		}
	}
}
