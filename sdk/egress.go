package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Dialer opens connections for protocol traffic. Network is tcp, tcp4, tcp6, udp, udp4 or udp6; a UDP connection
// sends every Write to address and returns every datagram the association receives.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// PacketDialer opens unconnected UDP sockets whose WriteTo may name any destination, host names included.
type PacketDialer interface {
	ListenPacket(ctx context.Context) (net.PacketConn, error)
}

// Egress dials through the Core's SOCKS5 (CONNECT for TCP, UDP ASSOCIATE for UDP, RFC 1929 auth). Host names are
// sent to the Core unresolved, so DNS policy stays with mihomo. It is the only network access an extension has.
type Egress struct {
	addr     string
	username string
	password string
}

var (
	_ Dialer       = (*Egress)(nil)
	_ PacketDialer = (*Egress)(nil)
)

// handshakeTimeout bounds a SOCKS5 negotiation whose context has no deadline.
const handshakeTimeout = 30 * time.Second

// NewEgress returns the dialer for the egress Core gave in the run request.
func NewEgress(c EgressConfig) *Egress {
	return &Egress{addr: c.SOCKS5, username: c.Username, password: c.Password}
}

// DialContext dials address through the egress.
func (e *Egress) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	target, err := ParseAddr(address)
	if err != nil {
		return nil, err
	}
	switch network {
	case "tcp", "tcp4", "tcp6":
		conn, _, err := e.request(ctx, cmdConnect, target)
		if err != nil {
			return nil, fmt.Errorf("egress connect %s: %w", address, err)
		}
		return &egressConn{Conn: conn, remote: target}, nil
	case "udp", "udp4", "udp6":
		pc, err := e.associate(ctx)
		if err != nil {
			return nil, fmt.Errorf("egress udp %s: %w", address, err)
		}
		return &udpConn{packetConn: pc, remote: target}, nil
	}
	return nil, fmt.Errorf("egress: unsupported network %q", network)
}

// ListenPacket opens a UDP ASSOCIATE through the egress.
func (e *Egress) ListenPacket(ctx context.Context) (net.PacketConn, error) {
	pc, err := e.associate(ctx)
	if err != nil {
		return nil, fmt.Errorf("egress udp: %w", err)
	}
	return pc, nil
}

func (e *Egress) associate(ctx context.Context) (*packetConn, error) {
	ctrl, bound, err := e.request(ctx, cmdUDPAssociate, Addr{Host: "0.0.0.0", Port: 0})
	if err != nil {
		return nil, err
	}
	relayIP := net.ParseIP(bound.Host)
	if relayIP == nil || relayIP.IsUnspecified() {
		relayIP = ctrl.RemoteAddr().(*net.TCPAddr).IP
	}
	relay := &net.UDPAddr{IP: relayIP, Port: int(bound.Port)}
	udp, err := net.DialUDP("udp", nil, relay)
	if err != nil {
		_ = ctrl.Close()
		return nil, err
	}
	pc := &packetConn{ctrl: ctrl, udp: udp}
	// The association lives as long as its TCP connection (RFC 1928 §7).
	go func() {
		_, _ = io.Copy(io.Discard, ctrl)
		_ = pc.Close()
	}()
	return pc, nil
}

// request connects to the egress, authenticates and sends one command; it returns the control connection and
// BND.ADDR of the reply.
func (e *Egress) request(ctx context.Context, cmd byte, target Addr) (net.Conn, Addr, error) {
	if e.addr == "" {
		return nil, Addr{}, errors.New("no egress configured")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", e.addr)
	if err != nil {
		return nil, Addr{}, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(handshakeTimeout)
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	bound, err := e.handshake(conn, cmd, target)
	if !stop() || err != nil {
		_ = conn.Close()
		if err == nil || ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		return nil, Addr{}, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, bound, nil
}

func (e *Egress) handshake(conn net.Conn, cmd byte, target Addr) (Addr, error) {
	method := byte(methodNoAuth)
	if e.username != "" || e.password != "" {
		method = methodUserPass
	}
	if _, err := conn.Write([]byte{socksVersion, 1, method}); err != nil {
		return Addr{}, err
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return Addr{}, err
	}
	if reply[0] != socksVersion || reply[1] != method {
		return Addr{}, errors.New("socks5: egress refused the authentication method")
	}
	if method == methodUserPass {
		if len(e.username) > 255 || len(e.password) > 255 {
			return Addr{}, errors.New("socks5: credentials too long")
		}
		b := []byte{1, byte(len(e.username))}
		b = append(b, e.username...)
		b = append(b, byte(len(e.password)))
		b = append(b, e.password...)
		if _, err := conn.Write(b); err != nil {
			return Addr{}, err
		}
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return Addr{}, err
		}
		if reply[1] != 0 {
			return Addr{}, errors.New("socks5: egress rejected the credentials")
		}
	}
	req, err := appendAddr([]byte{socksVersion, cmd, 0}, target)
	if err != nil {
		return Addr{}, err
	}
	if _, err := conn.Write(req); err != nil {
		return Addr{}, err
	}
	var head [3]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return Addr{}, err
	}
	if head[0] != socksVersion {
		return Addr{}, errors.New("socks5: bad reply version")
	}
	if head[1] != repSucceeded {
		return Addr{}, replyError(head[1])
	}
	return readAddr(conn)
}

// ReplyError is a SOCKS5 reply code other than success.
type ReplyError byte

func replyError(code byte) error { return ReplyError(code) }

func (e ReplyError) Error() string {
	switch byte(e) {
	case repGeneralFailure:
		return "socks5: general failure"
	case 0x02:
		return "socks5: connection not allowed by ruleset"
	case repNetworkUnreachable:
		return "socks5: network unreachable"
	case repHostUnreachable:
		return "socks5: host unreachable"
	case repConnectionRefused:
		return "socks5: connection refused"
	case 0x06:
		return "socks5: TTL expired"
	case repCommandNotSupported:
		return "socks5: command not supported"
	case repAddressNotSupported:
		return "socks5: address type not supported"
	}
	return fmt.Sprintf("socks5: reply %#x", byte(e))
}

// egressConn reports the requested target as its remote address instead of the egress itself.
type egressConn struct {
	net.Conn
	remote Addr
}

func (c *egressConn) RemoteAddr() net.Addr { return c.remote }

func (c *egressConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// maxDatagram is the largest UDP payload plus the largest SOCKS5 header.
const maxDatagram = 65535 + 262

// packetConn is one UDP ASSOCIATE.
type packetConn struct {
	ctrl net.Conn
	udp  *net.UDPConn

	once sync.Once
	mu   sync.Mutex
	buf  []byte
}

func (c *packetConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf == nil {
		c.buf = make([]byte, maxDatagram)
	}
	for {
		n, err := c.udp.Read(c.buf)
		if err != nil {
			return 0, nil, err
		}
		src, payload, err := parseUDPHeader(c.buf[:n])
		if err != nil {
			continue
		}
		var from net.Addr = src
		if ip := net.ParseIP(src.Host); ip != nil {
			from = &net.UDPAddr{IP: ip, Port: int(src.Port)}
		}
		return copy(p, payload), from, nil
	}
}

func (c *packetConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	dst, err := addrOf(addr)
	if err != nil {
		return 0, err
	}
	b, err := appendUDPHeader(make([]byte, 0, len(p)+262), dst)
	if err != nil {
		return 0, err
	}
	if _, err := c.udp.Write(append(b, p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *packetConn) Close() error {
	c.once.Do(func() {
		_ = c.udp.Close()
		_ = c.ctrl.Close()
	})
	return nil
}

func (c *packetConn) LocalAddr() net.Addr                { return c.udp.LocalAddr() }
func (c *packetConn) SetDeadline(t time.Time) error      { return c.udp.SetDeadline(t) }
func (c *packetConn) SetReadDeadline(t time.Time) error  { return c.udp.SetReadDeadline(t) }
func (c *packetConn) SetWriteDeadline(t time.Time) error { return c.udp.SetWriteDeadline(t) }

// udpConn is a packetConn bound to one destination. Replies are not filtered by source: the egress may report the
// resolved address of a destination that was sent as a name.
type udpConn struct {
	*packetConn
	remote Addr
}

func (c *udpConn) Read(p []byte) (int, error) {
	n, _, err := c.ReadFrom(p)
	return n, err
}

func (c *udpConn) Write(p []byte) (int, error) { return c.WriteTo(p, c.remote) }

func (c *udpConn) RemoteAddr() net.Addr { return c.remote }
