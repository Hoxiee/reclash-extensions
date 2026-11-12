// SPDX-License-Identifier: WTFPL

package protect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/transport/v4"
)

// Egress replaces every socket of the client with one opened by the embedding host: HTTP, WebSocket, ICE, STUN,
// TURN and DNS. Dial takes host names unresolved; Resolver answers the names pion has to turn into addresses.
type Egress struct {
	Dial         func(ctx context.Context, network, address string) (net.Conn, error)
	ListenPacket func(ctx context.Context) (net.PacketConn, error)
	Resolver     *net.Resolver
}

//nolint:gochecknoglobals // like the protector, the egress covers every socket of the process
var egress atomic.Pointer[Egress]

// ErrEgressListen is returned for sockets an egress cannot open: TCP listeners and IPv6 UDP.
var ErrEgressListen = errors.New("protect: listening is not available through the egress")

// SetEgress routes every client socket through e; nil restores direct sockets.
func SetEgress(e *Egress) {
	egress.Store(e)
}

// HasEgress reports whether an egress is configured.
func HasEgress() bool {
	return egress.Load() != nil
}

func egressTransport(t *http.Transport) *http.Transport {
	if e := egress.Load(); e != nil {
		t.Proxy = nil
		t.DialContext = e.Dial
	}
	return t
}

func egressWebSocket(d websocket.Dialer) websocket.Dialer {
	if e := egress.Load(); e != nil {
		d.Proxy = nil
		d.NetDialContext = e.Dial
	}
	return d
}

// egressIfaceAddr is the address pion announces for host candidates. The egress owns the real local address, so
// the peer reaches this one only as the peer-reflexive address it observes.
//
//nolint:gochecknoglobals // fixed placeholder (TEST-NET-1)
var egressIfaceAddr = net.IPv4(192, 0, 2, 1).To4()

// NewEgressNet returns pion's Net over the configured egress, or nil when there is none.
func NewEgressNet() transport.Net {
	e := egress.Load()
	if e == nil {
		return nil
	}
	return &egressNet{ProtectedNet: &ProtectedNet{resolver: e.Resolver}, e: e}
}

// egressNet keeps ProtectedNet only for name resolution, which goes to the egress resolver; every method that opens
// a socket is overridden.
type egressNet struct {
	*ProtectedNet
	e *Egress
}

func (n *egressNet) Interfaces() ([]*transport.Interface, error) {
	ifc := transport.NewInterface(net.Interface{
		Index: 1, MTU: 1500, Name: "egress0", Flags: net.FlagUp | net.FlagRunning,
	})
	ifc.AddAddress(&net.IPNet{IP: egressIfaceAddr, Mask: net.CIDRMask(32, 32)})
	return []*transport.Interface{ifc}, nil
}

func (n *egressNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	ifcs, _ := n.Interfaces()
	if index == ifcs[0].Index {
		return ifcs[0], nil
	}
	return nil, fmt.Errorf("%w: index=%d", transport.ErrInterfaceNotFound, index)
}

func (n *egressNet) InterfaceByName(name string) (*transport.Interface, error) {
	ifcs, _ := n.Interfaces()
	if name == ifcs[0].Name {
		return ifcs[0], nil
	}
	return nil, fmt.Errorf("%w: %s", transport.ErrInterfaceNotFound, name)
}

func (n *egressNet) ListenPacket(network, address string) (net.PacketConn, error) {
	var local *net.UDPAddr
	if host, _, err := net.SplitHostPort(address); err == nil {
		local = &net.UDPAddr{IP: net.ParseIP(host)}
	}
	return n.listenUDP(network, local)
}

func (n *egressNet) ListenUDP(network string, locAddr *net.UDPAddr) (transport.UDPConn, error) {
	return n.listenUDP(network, locAddr)
}

func (n *egressNet) listenUDP(network string, locAddr *net.UDPAddr) (*egressUDPConn, error) {
	if network != "udp" && network != "udp4" {
		return nil, fmt.Errorf("%w: %s", ErrEgressListen, network)
	}
	pc, err := n.e.ListenPacket(context.Background())
	if err != nil {
		return nil, fmt.Errorf("egress listen udp: %w", err)
	}
	return newEgressUDPConn(pc, locAddr, nil), nil
}

func (n *egressNet) ListenTCP(network string, _ *net.TCPAddr) (transport.TCPListener, error) {
	return nil, fmt.Errorf("%w: %s", ErrEgressListen, network)
}

func (n *egressNet) Dial(network, address string) (net.Conn, error) {
	return n.dial(context.Background(), network, address)
}

func (n *egressNet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := n.e.Dial(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("egress dial %s %q: %w", network, address, err)
	}
	if !strings.HasPrefix(network, "udp") {
		return conn, nil
	}
	pc, ok := conn.(net.PacketConn)
	if !ok {
		_ = conn.Close()
		return nil, ErrUnexpectedConnType
	}
	return newEgressUDPConn(pc, nil, conn.RemoteAddr()), nil
}

func (n *egressNet) DialUDP(network string, _, raddr *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.dial(context.Background(), network, udpAddrString(raddr))
	if err != nil {
		return nil, err
	}
	return conn.(*egressUDPConn), nil //nolint:forcetypeassert // dial wraps every udp conn
}

func (n *egressNet) DialTCP(network string, _, raddr *net.TCPAddr) (transport.TCPConn, error) {
	conn, err := n.dial(context.Background(), network, tcpAddrString(raddr))
	if err != nil {
		return nil, err
	}
	return egressTCPConn{conn}, nil
}

func (n *egressNet) CreateDialer(d *net.Dialer) transport.Dialer {
	timeout := time.Duration(0)
	if d != nil {
		timeout = d.Timeout
	}
	return egressDialer{n: n, timeout: timeout}
}

func (n *egressNet) CreateListenConfig(*net.ListenConfig) transport.ListenConfig {
	return egressListenConfig{n: n}
}

type egressDialer struct {
	n       *egressNet
	timeout time.Duration
}

func (d egressDialer) Dial(network, address string) (net.Conn, error) {
	ctx := context.Background()
	if d.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	return d.n.dial(ctx, network, address)
}

type egressListenConfig struct {
	n *egressNet
}

func (l egressListenConfig) Listen(_ context.Context, network, _ string) (net.Listener, error) {
	return nil, fmt.Errorf("%w: %s", ErrEgressListen, network)
}

func (l egressListenConfig) ListenPacket(_ context.Context, network, address string) (net.PacketConn, error) {
	return l.n.ListenPacket(network, address)
}

// egressUDPConn adapts an egress packet conn to pion's UDPConn. The local address keeps the bind IP pion asked for
// with the egress socket's port, since pion builds host candidates from it.
type egressUDPConn struct {
	net.PacketConn
	local  *net.UDPAddr
	remote net.Addr
}

func newEgressUDPConn(pc net.PacketConn, bind *net.UDPAddr, remote net.Addr) *egressUDPConn {
	local := &net.UDPAddr{IP: egressIfaceAddr}
	if bind != nil && bind.IP != nil && !bind.IP.IsUnspecified() {
		local.IP = bind.IP
	}
	if a, ok := pc.LocalAddr().(*net.UDPAddr); ok {
		local.Port = a.Port
	}
	return &egressUDPConn{PacketConn: pc, local: local, remote: remote}
}

func (c *egressUDPConn) LocalAddr() net.Addr { return c.local }

func (c *egressUDPConn) RemoteAddr() net.Addr { return c.remote }

func (c *egressUDPConn) SetReadBuffer(int) error { return nil }

func (c *egressUDPConn) SetWriteBuffer(int) error { return nil }

func (c *egressUDPConn) Read(b []byte) (int, error) {
	n, _, err := c.ReadFrom(b)
	return n, err
}

func (c *egressUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if ua, ok := addr.(*net.UDPAddr); ok {
		if ip4 := ua.IP.To4(); ip4 != nil {
			addr = &net.UDPAddr{IP: ip4, Port: ua.Port}
		}
	}
	return n, addr, err //nolint:wrapcheck // pion inspects net errors from its socket
}

func (c *egressUDPConn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	n, addr, err := c.ReadFrom(b)
	ua, _ := addr.(*net.UDPAddr)
	return n, ua, err
}

func (c *egressUDPConn) ReadMsgUDP(b, _ []byte) (int, int, int, *net.UDPAddr, error) {
	n, addr, err := c.ReadFromUDP(b)
	return n, 0, 0, addr, err
}

func (c *egressUDPConn) Write(b []byte) (int, error) {
	if c.remote == nil {
		return 0, &net.OpError{Op: "write", Net: "udp", Err: errors.New("not connected")}
	}
	return c.WriteTo(b, c.remote)
}

func (c *egressUDPConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	return c.WriteTo(b, addr)
}

func (c *egressUDPConn) WriteMsgUDP(b, _ []byte, addr *net.UDPAddr) (int, int, error) {
	n, err := c.WriteTo(b, addr)
	return n, 0, err
}

// egressTCPConn gives an egress stream the TCPConn surface pion asks for TURN over TCP; socket options have no
// socket to apply to.
type egressTCPConn struct {
	net.Conn
}

func (c egressTCPConn) CloseRead() error { return nil }

func (c egressTCPConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite() //nolint:wrapcheck // passthrough
	}
	return c.Conn.Close() //nolint:wrapcheck // passthrough
}

func (c egressTCPConn) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{c.Conn}, r) //nolint:wrapcheck // passthrough
}

func (c egressTCPConn) SetLinger(int) error { return nil }

func (c egressTCPConn) SetKeepAlive(bool) error { return nil }

func (c egressTCPConn) SetKeepAlivePeriod(time.Duration) error { return nil }

func (c egressTCPConn) SetNoDelay(bool) error { return nil }

func (c egressTCPConn) SetWriteBuffer(int) error { return nil }

func (c egressTCPConn) SetReadBuffer(int) error { return nil }

var _ transport.Net = (*egressNet)(nil)
