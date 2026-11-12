package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Hoxiee/reclash-extensions/sdk"
	"github.com/gorilla/websocket"
	olcclient "github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

// The provider and TURN names are resolved neither by the host, whose resolver is ReClash's own (fake-ip under
// TUN), nor over port 53, which networks and TUN stacks rewrite: Yandex DNS is asked over HTTPS through the egress.
const (
	dohURL        = "https://77.88.8.8/dns-query"
	dohServerName = "common.dot.dns.yandex.net"
	dohTimeout    = 10 * time.Second
	maxDNSMessage = 65535
)

// useEgress makes d the only network of the process: the patched upstream sockets, and the defaults its
// third-party libraries fall back to when they are not handed a client.
func useEgress(d *sdk.Egress) *net.Resolver {
	resolver := egressResolver(d)
	olcclient.SetEgress(&olcclient.Egress{Dial: d.DialContext, ListenPacket: d.ListenPacket, Resolver: resolver})
	net.DefaultResolver = resolver
	http.DefaultTransport = &http.Transport{
		DialContext: d.DialContext, ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second,
	}
	http.DefaultClient.Transport = http.DefaultTransport
	websocket.DefaultDialer.Proxy = nil
	websocket.DefaultDialer.NetDialContext = d.DialContext
	return resolver
}

func egressResolver(d sdk.Dialer) *net.Resolver {
	client := &http.Client{
		Timeout: dohTimeout,
		Transport: &http.Transport{
			DialContext:       d.DialContext,
			TLSClientConfig:   &tls.Config{ServerName: dohServerName, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: true,
			IdleConnTimeout:   90 * time.Second,
		},
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return newDoHConn(client), nil
		},
	}
}

// dohConn is the Go resolver's DNS connection answered over DoH. It does not implement net.PacketConn, so the
// resolver frames every message with a two-byte length as over TCP; each complete query becomes one POST.
type dohConn struct {
	client *http.Client

	mu       sync.Mutex
	cond     *sync.Cond
	out      bytes.Buffer
	in       bytes.Buffer
	deadline time.Time
	closed   bool
	err      error
	ctx      context.Context
	cancel   context.CancelFunc
}

func newDoHConn(client *http.Client) *dohConn {
	c := &dohConn{client: client}
	c.cond = sync.NewCond(&c.mu)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return c
}

func (c *dohConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.out.Write(b)
	var queries [][]byte
	for c.out.Len() >= 2 {
		size := int(binary.BigEndian.Uint16(c.out.Bytes()))
		if c.out.Len() < 2+size {
			break
		}
		c.out.Next(2)
		queries = append(queries, bytes.Clone(c.out.Next(size)))
	}
	deadline := c.deadline
	c.mu.Unlock()
	for _, q := range queries {
		go c.exchange(q, deadline)
	}
	return len(b), nil
}

func (c *dohConn) exchange(query []byte, deadline time.Time) {
	ctx := c.ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	answer, err := c.post(ctx, query)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.err == nil {
			c.err = err
		}
	} else {
		var size [2]byte
		binary.BigEndian.PutUint16(size[:], uint16(len(answer)))
		c.in.Write(size[:])
		c.in.Write(answer)
	}
	c.cond.Broadcast()
}

func (c *dohConn) post(ctx context.Context, query []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dohURL, bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh: http %d", resp.StatusCode)
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSMessage+1))
	if err != nil {
		return nil, fmt.Errorf("doh: %w", err)
	}
	if len(answer) == 0 || len(answer) > maxDNSMessage {
		return nil, errors.New("doh: bad answer size")
	}
	return answer, nil
}

func (c *dohConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var timer *time.Timer
	for c.in.Len() == 0 {
		switch {
		case c.closed:
			return 0, net.ErrClosed
		case c.err != nil:
			return 0, c.err
		case !c.deadline.IsZero() && !time.Now().Before(c.deadline):
			return 0, os.ErrDeadlineExceeded
		}
		if timer == nil && !c.deadline.IsZero() {
			timer = time.AfterFunc(time.Until(c.deadline), func() {
				c.mu.Lock()
				c.cond.Broadcast()
				c.mu.Unlock()
			})
			defer timer.Stop()
		}
		c.cond.Wait()
	}
	return c.in.Read(b)
}

func (c *dohConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
	c.cancel()
	return nil
}

func (c *dohConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *dohConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *dohConn) SetWriteDeadline(t time.Time) error { return c.SetDeadline(t) }

func (c *dohConn) LocalAddr() net.Addr  { return dohAddr{} }
func (c *dohConn) RemoteAddr() net.Addr { return dohAddr{} }

type dohAddr struct{}

func (dohAddr) Network() string { return "tcp" }
func (dohAddr) String() string  { return dohURL }
