package main

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// wgPipe joins the userspace WireGuard device to upstream's Dispatcher in memory. Upstream connects the two over a
// UDP socket on 127.0.0.1; here the device's Bind and the Dispatcher's PacketConn are the two ends of one pipe, so
// the extension opens no socket besides its front and the egress.
type wgPipe struct {
	toDispatcher chan []byte
	toDevice     chan []byte
	done         chan struct{}
	closeOnce    sync.Once

	mu       sync.Mutex
	bindDone chan struct{} // closed by Bind.Close; nil while the Bind is closed
}

// wgPipeAddr is the address both ends report; nothing is ever sent to it.
var wgPipeAddr = netip.MustParseAddrPort("127.0.0.1:9")

const wgPipeQueue = 1024

func newWGPipe() *wgPipe {
	return &wgPipe{
		toDispatcher: make(chan []byte, wgPipeQueue),
		toDevice:     make(chan []byte, wgPipeQueue),
		done:         make(chan struct{}),
	}
}

func (p *wgPipe) Close() error {
	p.closeOnce.Do(func() { close(p.done) })
	return nil
}

// bind is the device end.
func (p *wgPipe) bind() conn.Bind { return (*pipeBind)(p) }

// packetConn is the Dispatcher end.
func (p *wgPipe) packetConn() net.PacketConn { return (*pipeConn)(p) }

type pipeBind wgPipe

type pipeEndpoint struct{}

func (pipeEndpoint) ClearSrc()           {}
func (pipeEndpoint) SrcToString() string { return "" }
func (pipeEndpoint) DstToString() string { return wgPipeAddr.String() }
func (pipeEndpoint) DstToBytes() []byte  { b, _ := wgPipeAddr.MarshalBinary(); return b }
func (pipeEndpoint) DstIP() netip.Addr   { return wgPipeAddr.Addr() }
func (pipeEndpoint) SrcIP() netip.Addr   { return netip.Addr{} }

func (b *pipeBind) Open(uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bindDone != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	done := make(chan struct{})
	b.bindDone = done
	receive := func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		var pkt []byte
		select {
		case pkt = <-b.toDevice:
		case <-done:
			return 0, net.ErrClosed
		case <-b.done:
			return 0, net.ErrClosed
		}
		n := 0
		for {
			sizes[n] = copy(packets[n], pkt)
			eps[n] = pipeEndpoint{}
			n++
			if n == len(packets) {
				return n, nil
			}
			select {
			case pkt = <-b.toDevice:
			default:
				return n, nil
			}
		}
	}
	return []conn.ReceiveFunc{receive}, uint16(wgPipeAddr.Port()), nil
}

func (b *pipeBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bindDone != nil {
		close(b.bindDone)
		b.bindDone = nil
	}
	return nil
}

func (*pipeBind) SetMark(uint32) error { return nil }

func (*pipeBind) BatchSize() int { return 16 }

func (*pipeBind) ParseEndpoint(string) (conn.Endpoint, error) { return pipeEndpoint{}, nil }

func (b *pipeBind) Send(bufs [][]byte, _ conn.Endpoint) error {
	for _, buf := range bufs {
		select {
		case b.toDispatcher <- append([]byte(nil), buf...):
		case <-b.done:
			return net.ErrClosed
		}
	}
	return nil
}

type pipeConn wgPipe

var errPipeTimeout = errors.New("wgpipe: deadlines are not supported")

func (c *pipeConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pkt := <-c.toDispatcher:
		return copy(p, pkt), net.UDPAddrFromAddrPort(wgPipeAddr), nil
	case <-c.done:
		return 0, nil, net.ErrClosed
	}
}

func (c *pipeConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	select {
	case c.toDevice <- append([]byte(nil), p...):
		return len(p), nil
	case <-c.done:
		return 0, net.ErrClosed
	}
}

func (c *pipeConn) Close() error                   { return (*wgPipe)(c).Close() }
func (c *pipeConn) LocalAddr() net.Addr            { return net.UDPAddrFromAddrPort(wgPipeAddr) }
func (*pipeConn) SetDeadline(time.Time) error      { return errPipeTimeout }
func (*pipeConn) SetReadDeadline(time.Time) error  { return errPipeTimeout }
func (*pipeConn) SetWriteDeadline(time.Time) error { return errPipeTimeout }
