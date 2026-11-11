package main

import (
	"errors"
	"fmt"
	"net"

	"github.com/Hoxiee/reclash-extensions/sdk"
	"github.com/p1neappleXpress/OpenFlux/share"
)

// supportedTypes are the carriers whose every socket goes through netbind or the standard library defaults
// netbind.SetDial redirects (see patches/README.md). telemost is left out: its media runs over WebRTC's own UDP
// sockets, which nothing redirects.
var supportedTypes = map[string]bool{
	"direct": true, "yandex": true, "vyandex": true, "boards": true,
	"mailru": true, "cupsonline": true, "avito": true,
}

// decodeLink reads an openflux:// link with upstream's parser and refuses what this wrapper cannot run.
func decodeLink(link string) (share.Config, error) {
	c, err := share.Decode(link)
	if err != nil {
		return share.Config{}, err
	}
	if c.Mode != "" {
		return share.Config{}, fmt.Errorf("link: mode %q (PHP exit) is not supported", c.Mode)
	}
	if c.Secret == "" {
		return share.Config{}, errors.New("link: a link without an encryption secret is not supported")
	}
	for _, t := range c.Transports {
		if !supportedTypes[t.Type] {
			return share.Config{}, fmt.Errorf("link: transport %q is not supported", t.Type)
		}
	}
	return c, nil
}

// secretsOf is what a link carries that must not reach the log: the key, the context and the document URLs.
func secretsOf(c share.Config) []string {
	out := []string{c.Secret, c.Context}
	for _, t := range c.Transports {
		out = append(out, t.URL)
	}
	return out
}

func describeLink(link string) (sdk.Node, error) {
	c, err := decodeLink(link)
	if err != nil {
		return sdk.Node{}, err
	}
	// The client's gVisor stack carries UDP to the exit, which relays it (Session capability UDP).
	node := sdk.Node{Name: c.Name, UDP: true}
	for _, t := range c.Transports {
		if t.Type != "direct" {
			continue
		}
		node.Server = t.Dial
		if host, _, err := net.SplitHostPort(t.Dial); err == nil {
			node.Server = host
		}
		break
	}
	return node, nil
}
