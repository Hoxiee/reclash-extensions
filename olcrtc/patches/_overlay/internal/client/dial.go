// SPDX-License-Identifier: WTFPL

package client

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

// ErrClientStopped is returned by a Dialer whose client has stopped.
var ErrClientStopped = errors.New("client stopped")

// ErrConfig marks a RunDialer failure no retry can cure: a bad key or an unusable device id file.
var ErrConfig = errors.New("invalid client configuration")

// Dialer opens tunnel streams to targets the server connects to.
type Dialer interface {
	Dial(ctx context.Context, host string, port int) (net.Conn, error)
}

type streamDialer struct {
	c    *Client
	done <-chan struct{}
}

// RunDialer is RunWithAddress without the SOCKS listener: once the link is up, onReady gets a Dialer that opens
// streams directly. It blocks until ctx is canceled or the link ends.
func RunDialer(ctx context.Context, cfg Config, onReady func(Dialer)) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys, err := tunnelcore.SetupKeySet(cfg.KeyHex, crypto.Client)
	if err != nil {
		return fmt.Errorf("%w: setup key set: %w", ErrConfig, err)
	}
	deviceID, err := resolveDeviceID(cfg.DeviceID, cfg.DeviceIDPath)
	if err != nil {
		return fmt.Errorf("%w: resolve device id: %w", ErrConfig, err)
	}
	client := &Client{
		keys: keys, deviceID: deviceID, claims: cfg.Claims, dnsServer: cfg.DNSServer,
		health: runtime.NewHealthTracker(cfg.OnHealth), sessionReady: make(chan struct{}),
	}
	defer func() {
		cancel()
		client.shutdown()
	}()
	if bringUpErr := client.bringUpLink(runCtx, cfg, cancel); bringUpErr != nil {
		return bringUpErr
	}
	if onReady != nil {
		onReady(streamDialer{c: client, done: runCtx.Done()})
	}
	<-runCtx.Done()
	return nil
}

// Dial waits for a live session like handleSocks5 and sends the same connect request; the returned stream carries
// the target's bytes.
func (d streamDialer) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	for {
		session, sessionID, ready := d.c.sessionSnapshot()
		if session != nil && !session.IsClosed() && sessionID != "" {
			stream, err := session.OpenStream()
			if err != nil {
				return nil, fmt.Errorf("open stream: %w", err)
			}
			if err := d.c.sendConnectRequest(stream, host, port); err != nil {
				_ = stream.Close()
				return nil, err
			}
			return stream, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for session: %w", ctx.Err())
		case <-d.done:
			return nil, ErrClientStopped
		case <-ready:
		}
	}
}
