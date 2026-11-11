package sdk

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Proxy is the extension's side of the front: each SOCKS5 CONNECT becomes one DialTCP to "host:port", the host
// unresolved as the client sent it.
type Proxy interface {
	DialTCP(ctx context.Context, address string) (net.Conn, error)
}

// UDPProxy is a Proxy that also carries UDP. The front serves UDP ASSOCIATE only for one, and calls DialUDP once
// per destination of an association.
type UDPProxy interface {
	Proxy
	DialUDP(ctx context.Context, address string) (net.Conn, error)
}

const (
	frontHandshakeTimeout = 30 * time.Second
	frontDialTimeout      = 30 * time.Second
	udpFlowIdle           = 2 * time.Minute
	maxUDPFlows           = 256
)

// Front is the loopback SOCKS5 server Core connects to. It requires username/password auth with credentials
// generated for this process.
type Front struct {
	ln       net.Listener
	username string
	password string
	proxy    Proxy
	udp      UDPProxy

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[io.Closer]struct{}
	closed bool
}

// NewFront listens on listen, which must be a loopback address ("127.0.0.1:0" picks a port), and serves proxy.
func NewFront(listen string, proxy Proxy) (*Front, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("listen %q: the front binds loopback IP addresses only", listen)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	f := &Front{
		ln:       ln,
		username: "x-" + randomHex(4),
		password: randomToken(18),
		proxy:    proxy,
		conns:    make(map[io.Closer]struct{}),
	}
	f.udp, _ = proxy.(UDPProxy)
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.wg.Go(f.serve)
	return f, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Addr is the bound address, as reported in the ready event.
func (f *Front) Addr() string { return f.ln.Addr().String() }

func (f *Front) Username() string { return f.username }

func (f *Front) Password() string { return f.password }

// UDP reports whether the front serves UDP ASSOCIATE.
func (f *Front) UDP() bool { return f.udp != nil }

// Close stops listening and drops every connection.
func (f *Front) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	f.cancel()
	err := f.ln.Close()
	for c := range conns {
		_ = c.Close()
	}
	f.wg.Wait()
	return err
}

// track registers c to be closed by Close; false when the front is already closed.
func (f *Front) track(c io.Closer) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[c] = struct{}{}
	return true
}

func (f *Front) untrack(c io.Closer) {
	f.mu.Lock()
	delete(f.conns, c)
	f.mu.Unlock()
	_ = c.Close()
}

func (f *Front) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			if f.ctx.Err() != nil {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		if !f.track(conn) {
			_ = conn.Close()
			return
		}
		f.wg.Go(func() {
			defer f.untrack(conn)
			f.handle(conn)
		})
	}
}

func (f *Front) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(frontHandshakeTimeout))
	if !f.authenticate(conn) {
		return
	}
	var head [3]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil || head[0] != socksVersion {
		return
	}
	target, err := readAddr(conn)
	if err != nil {
		code := byte(repGeneralFailure)
		if errors.Is(err, errAddressType) {
			code = repAddressNotSupported
		}
		writeReply(conn, code, nil)
		return
	}
	switch head[1] {
	case cmdConnect:
		f.connect(conn, target)
	case cmdUDPAssociate:
		if f.udp == nil {
			writeReply(conn, repCommandNotSupported, nil)
			return
		}
		f.associate(conn)
	default:
		writeReply(conn, repCommandNotSupported, nil)
	}
}

func (f *Front) authenticate(conn net.Conn) bool {
	var greet [2]byte
	if _, err := io.ReadFull(conn, greet[:]); err != nil || greet[0] != socksVersion {
		return false
	}
	methods := make([]byte, greet[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return false
	}
	offered := false
	for _, m := range methods {
		offered = offered || m == methodUserPass
	}
	if !offered {
		_, _ = conn.Write([]byte{socksVersion, methodNoAcceptable})
		return false
	}
	if _, err := conn.Write([]byte{socksVersion, methodUserPass}); err != nil {
		return false
	}
	var ver [2]byte
	if _, err := io.ReadFull(conn, ver[:]); err != nil || ver[0] != 1 {
		return false
	}
	user := make([]byte, ver[1])
	if _, err := io.ReadFull(conn, user); err != nil {
		return false
	}
	var plen [1]byte
	if _, err := io.ReadFull(conn, plen[:]); err != nil {
		return false
	}
	pass := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, pass); err != nil {
		return false
	}
	okUser := subtle.ConstantTimeCompare(user, []byte(f.username))
	okPass := subtle.ConstantTimeCompare(pass, []byte(f.password))
	if okUser&okPass != 1 {
		_, _ = conn.Write([]byte{1, 1})
		return false
	}
	_, err := conn.Write([]byte{1, 0})
	return err == nil
}

// writeReply sends a reply with BND.ADDR set to bound, or 0.0.0.0:0.
func writeReply(conn net.Conn, code byte, bound net.Addr) {
	a := Addr{Host: "0.0.0.0"}
	if bound != nil {
		if b, err := addrOf(bound); err == nil {
			a = b
		}
	}
	b, err := appendAddr([]byte{socksVersion, code, 0}, a)
	if err != nil {
		return
	}
	_, _ = conn.Write(b)
}

func dialReplyCode(err error) byte {
	var re ReplyError
	if errors.As(err, &re) {
		return byte(re)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return repHostUnreachable
	}
	return repGeneralFailure
}

func (f *Front) connect(conn net.Conn, target Addr) {
	ctx, cancel := context.WithTimeout(f.ctx, frontDialTimeout)
	remote, err := f.proxy.DialTCP(ctx, target.String())
	cancel()
	if err != nil {
		writeReply(conn, dialReplyCode(err), nil)
		return
	}
	if !f.track(remote) {
		_ = remote.Close()
		return
	}
	defer f.untrack(remote)
	writeReply(conn, repSucceeded, conn.LocalAddr())
	_ = conn.SetDeadline(time.Time{})
	relay(conn, remote)
}

// relay copies both ways, passing a half-close on where the connection supports it.
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
			_ = src.Close()
		}
	}
	wg.Add(2)
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
}

// associate serves one UDP ASSOCIATE on a socket bound to the IP the client reached the front on; only datagrams
// from the control connection's IP are accepted, and the first one fixes the client's port.
func (f *Front) associate(ctrl net.Conn) {
	localIP := ctrl.LocalAddr().(*net.TCPAddr).IP
	clientIP := ctrl.RemoteAddr().(*net.TCPAddr).IP
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: localIP})
	if err != nil {
		writeReply(ctrl, repGeneralFailure, nil)
		return
	}
	if !f.track(pc) {
		_ = pc.Close()
		return
	}
	defer f.untrack(pc)
	writeReply(ctrl, repSucceeded, pc.LocalAddr())
	_ = ctrl.SetDeadline(time.Time{})

	a := &association{front: f, pc: pc, flows: make(map[string]net.Conn)}
	ctx, cancel := context.WithCancel(f.ctx)
	a.ctx = ctx
	go func() {
		_, _ = io.Copy(io.Discard, ctrl)
		cancel()
		_ = pc.Close()
	}()
	defer func() {
		cancel()
		a.closeFlows()
		a.wg.Wait()
	}()

	buf := make([]byte, maxDatagram)
	for {
		n, from, err := pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !from.IP.Equal(clientIP) {
			continue
		}
		if a.client == nil {
			a.client = from
		} else if a.client.Port != from.Port {
			continue
		}
		dst, payload, err := parseUDPHeader(buf[:n])
		if err != nil {
			continue
		}
		flow := a.flow(dst)
		if flow == nil {
			continue
		}
		_ = flow.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, _ = flow.Write(payload)
	}
}

type association struct {
	front  *Front
	ctx    context.Context
	pc     *net.UDPConn
	client *net.UDPAddr
	wg     sync.WaitGroup

	mu    sync.Mutex
	flows map[string]net.Conn
}

// flow returns the connection to dst, dialing it on first use.
func (a *association) flow(dst Addr) net.Conn {
	key := dst.String()
	a.mu.Lock()
	c, ok := a.flows[key]
	full := len(a.flows) >= maxUDPFlows
	a.mu.Unlock()
	if ok {
		return c
	}
	if full {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, frontDialTimeout)
	c, err := a.front.udp.DialUDP(ctx, key)
	cancel()
	if err != nil {
		return nil
	}
	a.mu.Lock()
	if a.ctx.Err() != nil {
		a.mu.Unlock()
		_ = c.Close()
		return nil
	}
	a.flows[key] = c
	a.mu.Unlock()
	client := a.client
	a.wg.Go(func() { a.pump(key, dst, c, client) })
	return c
}

// pump returns replies of one flow to the client until the flow is idle for udpFlowIdle.
func (a *association) pump(key string, dst Addr, c net.Conn, client *net.UDPAddr) {
	defer func() {
		a.mu.Lock()
		if a.flows[key] == c {
			delete(a.flows, key)
		}
		a.mu.Unlock()
		_ = c.Close()
	}()
	head, err := appendUDPHeader(nil, dst)
	if err != nil {
		return
	}
	out := make([]byte, len(head), len(head)+65535)
	copy(out, head)
	buf := make([]byte, 65535)
	for {
		_ = c.SetReadDeadline(time.Now().Add(udpFlowIdle))
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		if _, err := a.pc.WriteToUDP(append(out[:len(head)], buf[:n]...), client); err != nil {
			return
		}
	}
}

func (a *association) closeFlows() {
	a.mu.Lock()
	flows := a.flows
	a.flows = map[string]net.Conn{}
	a.mu.Unlock()
	for _, c := range flows {
		_ = c.Close()
	}
}
