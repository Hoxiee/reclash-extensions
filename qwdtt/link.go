package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Hoxiee/reclash-extensions/sdk"
)

// defaultWorkers is the Android client's default for a link without workers.
const defaultWorkers = 9

// config is a qwdtt://config?... link, read the way upstream's Android client reads it (SubscriptionImport).
type config struct {
	Name    string
	Peer    string
	Pass    string
	Hashes  []string
	Workers int
	// Port is the client's local WireGuard port; the server writes it into the WireGuard endpoint it hands out.
	Port string
}

func decodeLink(link string) (config, error) {
	link = strings.TrimSpace(link)
	scheme, rest, ok := strings.Cut(link, ":")
	if !ok || !strings.EqualFold(scheme, "qwdtt") {
		return config{}, errors.New("link: not a qwdtt link")
	}
	// The Android client also accepts the opaque form qwdtt:config?...
	if !strings.HasPrefix(rest, "//") {
		rest = "//" + rest
	}
	u, err := url.Parse("qwdtt:" + rest)
	if err != nil {
		return config{}, errors.New("link: malformed")
	}
	if !strings.EqualFold(u.Host, "config") || strings.Trim(u.Path, "/") != "" {
		return config{}, errors.New("link: only qwdtt://config?... links are supported")
	}
	q := u.Query()
	c := config{Name: q.Get("name"), Peer: q.Get("peer"), Pass: q.Get("pass"), Workers: defaultWorkers, Port: "9000"}
	if c.Pass == "" {
		c.Pass = q.Get("password")
	}
	if c.Pass == "" {
		return config{}, errors.New("link: no connection password")
	}
	host, port, err := net.SplitHostPort(c.Peer)
	if err != nil || host == "" {
		return config{}, errors.New("link: peer must be host:port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return config{}, errors.New("link: peer port is invalid")
	}
	c.Hashes = ParseHashes(q.Get("hashes"))
	if len(c.Hashes) == 0 {
		return config{}, errors.New("link: no VK call hashes")
	}
	if v := q.Get("workers"); v != "" {
		if c.Workers, err = strconv.Atoi(v); err != nil || c.Workers < 1 {
			return config{}, errors.New("link: workers is invalid")
		}
	}
	if v := q.Get("port"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
			return config{}, errors.New("link: port is invalid")
		}
		c.Port = v
	}
	if m := q.Get("mode"); m != "" && !strings.EqualFold(m, "vpn") && !strings.EqualFold(m, "wg") {
		return config{}, fmt.Errorf("link: mode %q is not supported", m)
	}
	return c, nil
}

// secretsOf is what a link carries that must not reach the log: the connection password and the call hashes.
func secretsOf(c config, link string) []string {
	out := []string{c.Pass}
	out = append(out, c.Hashes...)
	if u, err := url.Parse(strings.TrimSpace(link)); err == nil {
		out = append(out, u.Query().Get("hashes"), u.RawQuery)
	}
	return out
}

func describeLink(link string) (sdk.Node, error) {
	c, err := decodeLink(link)
	if err != nil {
		return sdk.Node{}, err
	}
	host, _, _ := net.SplitHostPort(c.Peer)
	return sdk.Node{Name: c.Name, Server: host, UDP: true}, nil
}
