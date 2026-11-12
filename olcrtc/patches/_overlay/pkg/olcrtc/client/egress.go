// SPDX-License-Identifier: WTFPL

package client

import (
	"context"
	"fmt"

	internalclient "github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// Egress is the host-provided network every client socket uses.
type Egress = protect.Egress

// Dialer opens tunnel streams on the established session.
type Dialer = internalclient.Dialer

// ErrClientStopped is returned by a Dialer whose client has stopped.
var ErrClientStopped = internalclient.ErrClientStopped

// ErrConfig marks a RunDialer failure no retry can cure.
var ErrConfig = internalclient.ErrConfig

// SetEgress routes every socket of the process through e (signalling, ICE, STUN, TURN, DNS); nil restores direct
// sockets. Set it before New.
func SetEgress(e *Egress) {
	protect.SetEgress(e)
}

// SetVerbose enables upstream debug logging.
func SetVerbose(enabled bool) {
	logger.SetVerbose(enabled)
}

// RunDialer starts the client without a SOCKS listener and hands onReady a Dialer once the link is up. It blocks
// until ctx is canceled or the provider ends; LocalAddr and the SOCKS credentials are ignored.
func (c *Client) RunDialer(ctx context.Context, onReady func(Dialer)) error {
	if err := internalclient.RunDialer(ctx, toClientConfig(c.cfg), onReady); err != nil {
		return fmt.Errorf("client: %w", err)
	}
	return nil
}
