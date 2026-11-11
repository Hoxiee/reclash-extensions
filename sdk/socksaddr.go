package sdk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const (
	socksVersion = 0x05

	methodNoAuth       = 0x00
	methodUserPass     = 0x02
	methodNoAcceptable = 0xff

	cmdConnect      = 0x01
	cmdBind         = 0x02
	cmdUDPAssociate = 0x03

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSucceeded           = 0x00
	repGeneralFailure      = 0x01
	repNetworkUnreachable  = 0x03
	repHostUnreachable     = 0x04
	repConnectionRefused   = 0x05
	repCommandNotSupported = 0x07
	repAddressNotSupported = 0x08
)

// Addr is a SOCKS5 address: a host name or IP and a port. It implements net.Addr so packet connections can report
// sources the egress gave as names.
type Addr struct {
	Host string
	Port uint16
}

func (a Addr) Network() string { return "udp" }

func (a Addr) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(int(a.Port))) }

// ParseAddr splits "host:port".
func ParseAddr(address string) (Addr, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return Addr{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return Addr{}, fmt.Errorf("bad port in %q", address)
	}
	if host == "" {
		return Addr{}, fmt.Errorf("missing host in %q", address)
	}
	return Addr{Host: host, Port: uint16(p)}, nil
}

func addrOf(a net.Addr) (Addr, error) {
	switch v := a.(type) {
	case Addr:
		return v, nil
	case *Addr:
		return *v, nil
	case *net.UDPAddr:
		return Addr{Host: v.IP.String(), Port: uint16(v.Port)}, nil
	case *net.TCPAddr:
		return Addr{Host: v.IP.String(), Port: uint16(v.Port)}, nil
	}
	return ParseAddr(a.String())
}

// appendAddr appends ATYP, address and port.
func appendAddr(b []byte, a Addr) ([]byte, error) {
	if ip := net.ParseIP(a.Host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			b = append(b, atypIPv4)
			b = append(b, ip4...)
		} else {
			b = append(b, atypIPv6)
			b = append(b, ip.To16()...)
		}
	} else {
		if len(a.Host) > 255 {
			return nil, errors.New("socks5: host name too long")
		}
		b = append(b, atypDomain, byte(len(a.Host)))
		b = append(b, a.Host...)
	}
	return binary.BigEndian.AppendUint16(b, a.Port), nil
}

// readAddr reads ATYP, address and port from a stream.
func readAddr(r io.Reader) (Addr, error) {
	var atyp [1]byte
	if _, err := io.ReadFull(r, atyp[:]); err != nil {
		return Addr{}, err
	}
	var host string
	switch atyp[0] {
	case atypIPv4, atypIPv6:
		ip := make(net.IP, 4)
		if atyp[0] == atypIPv6 {
			ip = make(net.IP, 16)
		}
		if _, err := io.ReadFull(r, ip); err != nil {
			return Addr{}, err
		}
		host = ip.String()
	case atypDomain:
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return Addr{}, err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(r, name); err != nil {
			return Addr{}, err
		}
		host = string(name)
	default:
		return Addr{}, errAddressType
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return Addr{}, err
	}
	return Addr{Host: host, Port: binary.BigEndian.Uint16(port[:])}, nil
}

var errAddressType = errors.New("socks5: unsupported address type")

// parseUDPHeader splits a UDP ASSOCIATE datagram (RFC 1928 §7) into destination and payload. Fragments are
// rejected: neither side of reclash-ext sends them.
func parseUDPHeader(p []byte) (Addr, []byte, error) {
	if len(p) < 4 || p[0] != 0 || p[1] != 0 {
		return Addr{}, nil, errors.New("socks5: bad UDP header")
	}
	if p[2] != 0 {
		return Addr{}, nil, errors.New("socks5: UDP fragments are not supported")
	}
	r := &sliceReader{b: p[3:]}
	a, err := readAddr(r)
	if err != nil {
		return Addr{}, nil, err
	}
	return a, r.b, nil
}

func appendUDPHeader(b []byte, a Addr) ([]byte, error) {
	return appendAddr(append(b, 0, 0, 0), a)
}

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
