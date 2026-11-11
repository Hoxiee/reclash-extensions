// Package netbind keeps the core's own connections off its own tunnel.
//
// With the Windows full tunnel (--inbound=tun) the default route points at
// the OpenFlux adapter, but the carriers (Yandex, the node's direct port)
// must still leave through the real network, or they would loop into the
// tunnel they carry. Android gets this by excluding the app from its VPN;
// here every outbound socket of the core is bound to the physical
// interface instead (IP_UNICAST_IF), which holds whatever the routes say.
// Dial sites use Dialer or DialContext; Bind also covers the defaults
// (http.DefaultTransport, websocket.DefaultDialer, net.DefaultResolver).
//
// An embedder that runs the core behind a proxy (a SOCKS5 egress) instead
// hands every one of those dials to its own function with SetDial.
package netbind

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type controlFunc = func(network, address string, c syscall.RawConn) error

var current atomic.Pointer[controlFunc]

// DialFunc dials like net.Dialer.DialContext.
type DialFunc = func(ctx context.Context, network, address string) (net.Conn, error)

var redirect atomic.Pointer[DialFunc]

// control applies the current binding, if any.
func control(network, address string, c syscall.RawConn) error {
	if f := current.Load(); f != nil {
		return (*f)(network, address, c)
	}
	return nil
}

// BoundDialer is a net.Dialer that follows the binding, or dials through
// the function given to SetDial when there is one.
type BoundDialer struct {
	*net.Dialer
}

// DialContext dials through SetDial's function, within the dialer's
// Timeout, or else with the net.Dialer.
func (d *BoundDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if f := redirect.Load(); f != nil {
		if d.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d.Timeout)
			defer cancel()
		}
		return (*f)(ctx, network, address)
	}
	return d.Dialer.DialContext(ctx, network, address)
}

// Dial is DialContext without a context.
func (d *BoundDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// Dialer is a net.Dialer that follows the binding.
func Dialer(timeout time.Duration) *BoundDialer {
	return Wrap(&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second})
}

// Wrap makes d follow the binding (keeping its own Control, if any).
func Wrap(d *net.Dialer) *BoundDialer {
	own := d.Control
	d.Control = func(network, address string, c syscall.RawConn) error {
		if own != nil {
			if err := own(network, address, c); err != nil {
				return err
			}
		}
		return control(network, address, c)
	}
	return &BoundDialer{Dialer: d}
}

// DialContext dials with a default Dialer (30 s timeout).
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return Dialer(30*time.Second).DialContext(ctx, network, address)
}

// Bound reports whether sockets are bound to an interface now.
func Bound() bool { return current.Load() != nil }

// SetDial makes every dial of the core, the standard library's defaults
// included, go through f instead of the system network. Name resolution
// is then left to f: dial sites hand it host names as they are. nil
// restores direct dials.
func SetDial(f DialFunc) {
	if f == nil {
		redirect.Store(nil)
		return
	}
	redirect.Store(&f)
	installDefaults()
}

// Redirected reports whether dials go through SetDial's function.
func Redirected() bool { return redirect.Load() != nil }

// install sets the binding and points the standard library's defaults at
// bound dialers, so code that never names a dialer is covered too.
func install(f controlFunc) {
	current.Store(&f)
	installDefaults()
}

func installDefaults() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.DialContext = DialContext
	}
	websocket.DefaultDialer.NetDialContext = DialContext
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return Dialer(10*time.Second).DialContext(ctx, network, address)
		},
	}
}
