// SPDX-License-Identifier: WTFPL

package engine

import (
	"net"

	"github.com/pion/ice/v4"
	"github.com/pion/transport/v4"
	"github.com/pion/webrtc/v4"

	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// egressPionSettings applies the egress Net to every engine. ICE stays on UDP4: pion's active ICE-TCP binds and
// dials with the net package directly, past SetNet. TURN over TCP still goes through the ICE proxy dialer.
func egressPionSettings(opts PionSettingsOptions, egressNet transport.Net) PionSettings {
	return func(settings *webrtc.SettingEngine) {
		if opts.LoggerFactory != nil {
			settings.LoggerFactory = opts.LoggerFactory
		}
		settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		settings.SetIPFilter(func(ip net.IP) bool { return ip.To4() != nil })
		settings.SetNet(egressNet)
		settings.SetICEProxyDialer(protect.NewProxyDialer())
		settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}
}
