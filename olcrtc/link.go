package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Hoxiee/reclash-extensions/sdk"
	olcclient "github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

// Transport defaults are upstream's mobile runtime defaults (mobile/config.go), used for keys a link leaves out.
const (
	defaultVP8FPS          = 30
	defaultVP8Batch        = 64
	defaultSEIFPS          = 30
	defaultSEIBatch        = 64
	defaultSEIFragment     = 900
	defaultSEIAckMS        = 2000
	defaultVideoWidth      = 1920
	defaultVideoHeight     = 1080
	defaultVideoFPS        = 30
	defaultVideoCodec      = "qrcode"
	defaultVideoRecovery   = "low"
	defaultVideoTileModule = 4
	tileSide               = 1080
)

// serviceHosts is what a link of a fixed-service provider names as its server; a jitsi link names its own host.
var serviceHosts = map[string]string{
	"telemost": "telemost.yandex.ru",
	"wbstream": "stream.wb.ru",
}

var qrRecoveryLevels = []string{"low", "medium", "high", "highest"}

// config is one olcrtc:// link in upstream's client URI convention (docs/uri.md):
// olcrtc://<provider>?<transport>[<key=value&...>]@<room>#<key hex>[$<comment>].
type config struct {
	Name      string
	Provider  string
	Transport string
	Room      string
	KeyHex    string
	Server    string
	Options   olcclient.TransportOptions
}

func decodeLink(link string) (config, error) {
	link = strings.TrimSpace(link)
	scheme, body, ok := strings.Cut(link, "://")
	if !ok || !strings.EqualFold(scheme, "olcrtc") {
		return config{}, errors.New("link: not an olcrtc link")
	}
	provider, rest, ok := strings.Cut(body, "?")
	if !ok {
		return config{}, errors.New("link: no transport after ?")
	}
	transportPart, rest, ok := strings.Cut(rest, "@")
	if !ok {
		return config{}, errors.New("link: no room after @")
	}
	room, rest, ok := strings.Cut(rest, "#")
	if !ok {
		return config{}, errors.New("link: no key after #")
	}
	keyHex, comment, _ := strings.Cut(rest, "$")

	c := config{
		Provider: strings.ToLower(strings.TrimSpace(provider)),
		Room:     strings.TrimSpace(room),
		KeyHex:   strings.ToLower(strings.TrimSpace(keyHex)),
	}
	switch c.Provider {
	case "telemost", "wbstream":
		c.Server = serviceHosts[c.Provider]
	case "jitsi":
		host, err := jitsiHost(c.Room)
		if err != nil {
			return config{}, err
		}
		c.Server = host
	default:
		return config{}, fmt.Errorf("link: provider %q is not supported", provider)
	}
	if c.Room == "" {
		return config{}, errors.New("link: no room id")
	}
	if key, err := hex.DecodeString(c.KeyHex); err != nil || len(key) != 32 {
		return config{}, errors.New("link: key must be 64 hex characters")
	}

	name, params, err := splitTransport(transportPart)
	if err != nil {
		return config{}, err
	}
	c.Transport = name
	if c.Options, err = transportOptions(name, params); err != nil {
		return config{}, err
	}

	c.Name = strings.TrimSpace(comment)
	if decoded, err := url.PathUnescape(c.Name); err == nil {
		c.Name = strings.TrimSpace(decoded)
	}
	if c.Name == "" {
		c.Name = "olcrtc " + c.Provider
	}
	return c, nil
}

func jitsiHost(room string) (string, error) {
	raw := strings.TrimSpace(room)
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	raw = strings.TrimLeft(raw, "/")
	host, path, ok := strings.Cut(raw, "/")
	if !ok || host == "" || strings.Trim(path, "/") == "" {
		return "", errors.New("link: a jitsi room must be host/room or https://host/room")
	}
	return host, nil
}

func splitTransport(part string) (string, url.Values, error) {
	name, payload, hasPayload := strings.Cut(strings.TrimSpace(part), "<")
	name = strings.ToLower(strings.TrimSpace(name))
	params := url.Values{}
	if hasPayload {
		inner, ok := strings.CutSuffix(strings.TrimSpace(payload), ">")
		if !ok {
			return "", nil, errors.New("link: transport parameters are not closed with >")
		}
		var err error
		if params, err = url.ParseQuery(inner); err != nil {
			return "", nil, errors.New("link: malformed transport parameters")
		}
	}
	return name, params, nil
}

// transportOptions reads the payload keys docs/uri.md assigns to each transport. Keys it does not list, such as the
// retired video-bitrate and video-hw, are ignored like upstream ignores them.
func transportOptions(name string, p url.Values) (olcclient.TransportOptions, error) {
	r := paramReader{p: p}
	switch name {
	case "datachannel":
		return nil, nil
	case "vp8channel":
		o := olcclient.VP8Options{FPS: r.int("vp8-fps", defaultVP8FPS), BatchSize: r.int("vp8-batch", defaultVP8Batch)}
		return o, r.err
	case "seichannel":
		o := olcclient.SEIOptions{
			FPS: r.int("fps", defaultSEIFPS), BatchSize: r.int("batch", defaultSEIBatch),
			FragmentSize: r.int("frag", defaultSEIFragment), AckTimeoutMS: r.int("ack-ms", defaultSEIAckMS),
		}
		return o, r.err
	case "videochannel":
		return videoOptions(&r)
	case "":
		return nil, errors.New("link: no transport")
	default:
		return nil, fmt.Errorf("link: transport %q is not supported", name)
	}
}

func videoOptions(r *paramReader) (olcclient.TransportOptions, error) {
	codec := strings.ToLower(r.p.Get("video-codec"))
	if codec == "" {
		codec = defaultVideoCodec
	}
	width, height := defaultVideoWidth, defaultVideoHeight
	switch codec {
	case "qrcode":
	case "tile":
		width, height = tileSide, tileSide
	default:
		return nil, fmt.Errorf("link: video-codec %q is not supported", codec)
	}
	o := olcclient.VideoOptions{
		Width: r.int("video-w", width), Height: r.int("video-h", height), FPS: r.int("video-fps", defaultVideoFPS),
		Codec: codec, QRSize: r.optionalInt("video-qr-size"), QRRecovery: strings.ToLower(r.p.Get("video-qr-recovery")),
		TileModule: r.optionalInt("video-tile-module"), TileRS: r.optionalInt("video-tile-rs"),
	}
	if r.err != nil {
		return nil, r.err
	}
	if o.QRRecovery == "" {
		o.QRRecovery = defaultVideoRecovery
	} else if !contains(qrRecoveryLevels, o.QRRecovery) {
		return nil, fmt.Errorf("link: video-qr-recovery %q is not supported", o.QRRecovery)
	}
	if codec == "tile" {
		if o.Width != tileSide || o.Height != tileSide {
			return nil, errors.New("link: the tile codec needs 1080x1080 video")
		}
		if o.TileModule == 0 {
			o.TileModule = defaultVideoTileModule
		}
	}
	return o, nil
}

type paramReader struct {
	p   url.Values
	err error
}

// int reads a positive integer, or def when the key is absent.
func (r *paramReader) int(key string, def int) int {
	v := strings.TrimSpace(r.p.Get(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if r.err == nil {
			r.err = fmt.Errorf("link: %s must be a positive integer", key)
		}
		return def
	}
	return n
}

// optionalInt reads a non-negative integer whose zero leaves the choice to upstream.
func (r *paramReader) optionalInt(key string) int {
	v := strings.TrimSpace(r.p.Get(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		if r.err == nil {
			r.err = fmt.Errorf("link: %s must be a non-negative integer", key)
		}
		return 0
	}
	return n
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// secretsOf is what a link carries that must not reach the log: the key and the room, which admits anyone to the
// call carrying the tunnel.
func secretsOf(c config) []string {
	out := []string{c.KeyHex, c.Room}
	if strings.ToUpper(c.KeyHex) != c.KeyHex {
		out = append(out, strings.ToUpper(c.KeyHex))
	}
	if escaped := url.QueryEscape(c.Room); escaped != c.Room {
		out = append(out, escaped)
	}
	return out
}

func describeLink(link string) (sdk.Node, error) {
	c, err := decodeLink(link)
	if err != nil {
		return sdk.Node{}, err
	}
	// The server only opens TCP streams (its connect request); there is no UDP relay.
	return sdk.Node{Name: c.Name, Server: c.Server, UDP: false}, nil
}
