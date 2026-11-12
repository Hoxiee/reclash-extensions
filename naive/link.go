package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Hoxiee/reclash-extensions/sdk"
	"golang.org/x/net/http/httpguts"
)

const maxConcurrency = 8

type header struct{ name, value string }

type config struct {
	name     string
	host     string
	port     string
	username string
	password string
	auth     string
	extra    []header
	// conns is how many HTTP/2 connections the tunnels are spread over (naive's insecure-concurrency).
	conns int
}

func (c config) server() string { return net.JoinHostPort(c.host, c.port) }

// decodeLink reads a naive+https link (DuckSoft's NaiveProxy URI, as v2rayN and NekoBox write it).
func decodeLink(link string) (config, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return config{}, errors.New("link: not a URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "naive+https":
	case "naive+quic":
		return config{}, errors.New("link: naive+quic (HTTP/3) is not supported, only naive+https")
	default:
		return config{}, fmt.Errorf("link: scheme %q is not naive+https", u.Scheme)
	}
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return config{}, errors.New("link: a path is not allowed")
	}
	c := config{name: u.Fragment, host: u.Hostname(), port: u.Port(), conns: 1}
	if c.host == "" {
		return config{}, errors.New("link: no server host")
	}
	if c.port == "" {
		c.port = "443"
	} else if p, err := strconv.Atoi(c.port); err != nil || p < 1 || p > 65535 {
		return config{}, errors.New("link: bad server port")
	}
	if c.name == "" {
		c.name = c.host
	}
	if u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
		c.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password))
	}

	q := u.Query()
	// Old naive had a --padding switch; current naive negotiates padding with every server, and so does this client.
	if v := q.Get("padding"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return config{}, errors.New("link: padding is not true or false")
		}
	}
	if v := q.Get("insecure-concurrency"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return config{}, errors.New("link: bad insecure-concurrency")
		}
		c.conns = min(n, maxConcurrency)
	}
	if v := q.Get("extra-headers"); v != "" {
		if c.extra, err = parseExtraHeaders(v); err != nil {
			return config{}, err
		}
	}
	return c, nil
}

// reservedHeaders are set by the client itself.
var reservedHeaders = map[string]bool{
	"proxy-authorization": true, "padding": true, "padding-type-request": true, "fastopen": true,
	"host": true, "connection": true, "proxy-connection": true, "transfer-encoding": true, "upgrade": true,
	"keep-alive": true, "te": true, "content-length": true,
}

func parseExtraHeaders(s string) ([]header, error) {
	var out []header
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if !ok || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return nil, errors.New("link: bad extra-headers")
		}
		if reservedHeaders[name] {
			return nil, fmt.Errorf("link: extra header %q is set by the client", name)
		}
		out = append(out, header{name, value})
	}
	return out, nil
}

// secretsOf is what a link carries that must not reach the log.
func secretsOf(c config) []string {
	out := []string{c.username, c.password, c.auth, strings.TrimPrefix(c.auth, "Basic ")}
	for _, h := range c.extra {
		out = append(out, h.value)
	}
	return out
}

func describeLink(link string) (sdk.Node, error) {
	c, err := decodeLink(link)
	if err != nil {
		return sdk.Node{}, err
	}
	// naive carries UDP only over QUIC (naive+quic), which this extension does not run.
	return sdk.Node{Name: c.name, Server: c.host, UDP: false}, nil
}
