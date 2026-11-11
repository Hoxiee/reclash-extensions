package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	"github.com/pion/turn/v5"
	"golang.org/x/net/http2"
)

// dohPreset is upstream's Yandex DNS, asked over HTTPS: the VK and server names are resolved neither by the host,
// whose resolver is ReClash's own (fake-ip), nor over port 53, which networks and TUN stacks rewrite.
const dohPreset = "doh-yandex"

const dohTimeout = 10 * time.Second

const turnDialTimeout = 10 * time.Second

var errNoEgress = errors.New("qwdtt: no egress")

type egressRoute struct {
	ctx context.Context
	d   sdk.Dialer
}

var route atomic.Pointer[egressRoute]

// useEgress routes the patched VK API clients, the TURN sockets and net.DefaultResolver through d; ctx bounds the
// TURN dials, which upstream makes without a context.
func useEgress(ctx context.Context, d sdk.Dialer) {
	route.Store(&egressRoute{ctx: ctx, d: d})
	net.DefaultResolver = egressResolver(d)
}

func egressDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	r := route.Load()
	if r == nil {
		return nil, errNoEgress
	}
	return r.d.DialContext(ctx, network, address)
}

func egressResolver(d sdk.Dialer) *net.Resolver {
	endpoints := goDoHEndpointsForPreset(dohPreset)
	client := &http.Client{
		Timeout: dohTimeout,
		Transport: &http2.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if cfg = cfg.Clone(); cfg == nil {
					cfg = &tls.Config{}
				}
				cfg.ServerName = host
				if sni := dohHostHeader("https://" + host + "/"); sni != "" {
					cfg.ServerName = sni
				}
				c, err := d.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				tc := tls.Client(c, cfg)
				if err := tc.HandshakeContext(ctx); err != nil {
					_ = c.Close()
					return nil, err
				}
				return tc, nil
			},
		},
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return newDohConn(endpoints, client), nil
		},
	}
}

// dialTURNConn takes the place of upstream's, which the patch renames to directDialTURNConn.
func dialTURNConn(turnAddr string, tcp bool) (net.PacketConn, io.Closer, error) {
	r := route.Load()
	if r == nil {
		return nil, nil, errNoEgress
	}
	ctx, cancel := context.WithTimeout(r.ctx, turnDialTimeout)
	defer cancel()
	if tcp {
		c, err := r.d.DialContext(ctx, "tcp", turnAddr)
		if err != nil {
			return nil, nil, err
		}
		return turn.NewSTUNConn(c), c, nil
	}
	c, err := r.d.DialContext(ctx, "udp", turnAddr)
	if err != nil {
		return nil, nil, err
	}
	return connectedPacketConn{c}, c, nil
}

// connectedPacketConn hands pion a connected egress UDP conn: every write goes to the TURN server.
type connectedPacketConn struct{ net.Conn }

func (c connectedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Read(p)
	return n, c.RemoteAddr(), err
}

func (c connectedPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.Write(p) }
