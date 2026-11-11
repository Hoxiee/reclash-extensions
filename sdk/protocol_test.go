package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fixture struct {
	Protocol int             `json:"protocol"`
	Manifest json.RawMessage `json:"manifest"`
	Run      struct {
		Argv    []string          `json:"argv"`
		Request json.RawMessage   `json:"request"`
		Events  []json.RawMessage `json:"events"`
	} `json:"run"`
	Describe struct {
		Argv     []string        `json:"argv"`
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	} `json:"describe"`
	Version struct {
		Argv     []string        `json:"argv"`
		Response json.RawMessage `json:"response"`
	} `json:"version"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/protocol.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The SDK copy must stay byte-identical to the Core's fixture whenever both repositories are checked out side by
// side.
func TestFixtureMatchesCore(t *testing.T) {
	core, err := os.ReadFile("../../ReClash/core/extensions/testdata/protocol.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("ReClash checkout not found next to reclash-extensions")
	}
	if err != nil {
		t.Fatal(err)
	}
	ours, err := os.ReadFile("testdata/protocol.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(core, ours) {
		t.Fatal("sdk/testdata/protocol.json differs from ReClash/core/extensions/testdata/protocol.json")
	}
}

// roundTrip decodes raw into a new T, rejecting unknown fields, re-encodes it and checks that the two JSON
// documents carry the same values. A key the encoding leaves out (omitempty) must have been a zero value.
func roundTrip[T any](t *testing.T, name string, raw json.RawMessage) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: encode: %v", name, err)
	}
	var want, got any
	_ = json.Unmarshal(raw, &want)
	_ = json.Unmarshal(out, &got)
	if !reflect.DeepEqual(dropZero(want), dropZero(got)) {
		t.Fatalf("%s: re-encoded differently\nwant %s\ngot  %s", name, raw, out)
	}
	return v
}

func dropZero(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if e == nil || e == false || e == "" || e == float64(0) {
				continue
			}
			out[k] = dropZero(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = dropZero(e)
		}
		return out
	}
	return v
}

func TestFixtureRoundTrip(t *testing.T) {
	f := loadFixture(t)
	if f.Protocol != ProtocolVersion {
		t.Fatalf("fixture protocol %d, SDK %d", f.Protocol, ProtocolVersion)
	}
	if f.Run.Argv[0] != CommandRun || f.Describe.Argv[0] != CommandDescribe || f.Version.Argv[0] != CommandVersion {
		t.Fatalf("argv: %v %v %v", f.Run.Argv, f.Describe.Argv, f.Version.Argv)
	}
	m := roundTrip[Manifest](t, "manifest", f.Manifest)
	if m.Executable == "" || len(m.SHA256) != 64 {
		t.Fatalf("manifest: %+v", m)
	}
	req := roundTrip[RunRequest](t, "run.request", f.Run.Request)
	if req.Egress.SOCKS5 == "" || req.Egress.Password == "" {
		t.Fatalf("run request lost the egress: %+v", req)
	}
	kinds := map[string]bool{}
	for i, raw := range f.Run.Events {
		ev := roundTrip[Event](t, "run.events["+string(rune('0'+i))+"]", raw)
		kinds[ev.Event] = true
		if ev.Event == EventStatus && ev.State != StateOK && ev.State != StateDegraded {
			t.Fatalf("status state %q", ev.State)
		}
	}
	for _, k := range []string{EventReady, EventStatus, EventFatal} {
		if !kinds[k] {
			t.Fatalf("fixture has no %s event", k)
		}
	}
	roundTrip[DescribeRequest](t, "describe.request", f.Describe.Request)
	roundTrip[DescribeResponse](t, "describe.response", f.Describe.Response)
	roundTrip[VersionResponse](t, "version.response", f.Version.Response)
}

// fixtureExt answers describe and version the way the fixture expects.
type fixtureExt struct{}

func (fixtureExt) Info() Info { return Info{ID: "openflux", Version: "0.1.0"} }

func (fixtureExt) Describe(link string) (Node, error) {
	if strings.HasSuffix(link, "broken") {
		return Node{}, errors.New("link: invalid base64")
	}
	return Node{Name: "Finland", Server: "203.0.113.10"}, nil
}

func (fixtureExt) Start(context.Context, *Runtime) (Proxy, error) {
	return nil, errors.New("link: unsupported version 2")
}

func jsonEqual(t *testing.T, name string, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v: %s", name, err, got)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("%s:\nwant %s\ngot  %s", name, want, got)
	}
}

func TestFixtureCommands(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	var out, errOut bytes.Buffer
	if code := Run(ctx, fixtureExt{}, f.Version.Argv, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("version exit %d: %s", code, errOut.String())
	}
	jsonEqual(t, "version", f.Version.Response, out.Bytes())

	out.Reset()
	if code := Run(ctx, fixtureExt{}, f.Describe.Argv, bytes.NewReader(f.Describe.Request), &out, &errOut); code != 0 {
		t.Fatalf("describe exit %d: %s", code, errOut.String())
	}
	jsonEqual(t, "describe", f.Describe.Response, out.Bytes())

	// A start that fails ends in the fixture's fatal event and a non-zero exit.
	var req RunRequest
	_ = json.Unmarshal(f.Run.Request, &req)
	req.DataDir = t.TempDir()
	runReq, _ := json.Marshal(req)
	out.Reset()
	if code := Run(ctx, fixtureExt{}, f.Run.Argv, bytes.NewReader(runReq), &out, &errOut); code == 0 {
		t.Fatal("failed start exited 0")
	}
	jsonEqual(t, "fatal", f.Run.Events[len(f.Run.Events)-1], bytes.TrimSpace(out.Bytes()))
}
