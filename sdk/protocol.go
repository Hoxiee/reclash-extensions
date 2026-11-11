// Package sdk builds ReClash protocol extensions: executables that speak reclash-ext/1 to ReClash Core.
//
// An extension implements Extension and calls Main. The SDK owns the process contract (the run, describe and
// version commands, the JSON on stdin and stdout, log redaction on stderr), the loopback SOCKS5 front Core
// connects to, and the egress dialers every protocol socket must use.
//
// The wire types below mirror ReClash/core/extensions/protocol.go; testdata/protocol.json is the shared fixture.
package sdk

// ProtocolVersion is the reclash-ext protocol this SDK speaks.
const ProtocolVersion = 1

const (
	CommandRun      = "run"
	CommandDescribe = "describe"
	CommandVersion  = "version"
)

// Manifest is <core home>/extensions/<id>/<version>/manifest.json. Executable is relative to that directory and
// SHA256 is the lowercase hex digest of the executable.
type Manifest struct {
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	Protocol   []int    `json:"protocol"`
	Schemes    []string `json:"schemes"`
	Executable string   `json:"executable"`
	SHA256     string   `json:"sha256"`
}

// EgressConfig is the Core's loopback SOCKS5 endpoint every protocol socket must go through.
type EgressConfig struct {
	SOCKS5   string `json:"socks5"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// RunRequest is the single JSON object Core writes to the stdin of `<exe> run`. stdin EOF asks the extension to
// exit.
type RunRequest struct {
	Protocol int          `json:"protocol"`
	Link     string       `json:"link"`
	Listen   string       `json:"listen"`
	Egress   EgressConfig `json:"egress"`
	DataDir  string       `json:"dataDir"`
	LogLevel string       `json:"logLevel"`
}

const (
	EventReady  = "ready"
	EventStatus = "status"
	EventFatal  = "fatal"

	StateOK       = "ok"
	StateDegraded = "degraded"
)

// Event is one JSON line on the stdout of `<exe> run`.
type Event struct {
	Event    string `json:"event"`
	SOCKS5   string `json:"socks5,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	UDP      bool   `json:"udp,omitempty"`
	State    string `json:"state,omitempty"`
	Message  string `json:"message,omitempty"`
}

// DescribeRequest is the stdin of `<exe> describe`.
type DescribeRequest struct {
	Links []string `json:"links"`
}

// DescribeResponse has one node per requested link, in order.
type DescribeResponse struct {
	Nodes []DescribedNode `json:"nodes"`
}

type DescribedNode struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	UDP    bool   `json:"udp"`
	Error  string `json:"error"`
}

// VersionResponse is the stdout of `<exe> version`.
type VersionResponse struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Protocol []int  `json:"protocol"`
}
