package main

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"io"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"sync"

	"github.com/bogdanfinn/fhttp/http2/hpack"
)

// The padding protocol of naive (src/net/tools/naive/naive_protocol.h, naive_padding_framer.cc): when both sides
// agree on variant 1, the first 8 reads and writes of a tunnel are frames of
// [payload size, 2 bytes big-endian][padding size, 1 byte][payload][padding zeros], the padding size uniform in
// [0, 255]; everything after that is the bare stream.
const (
	paddingNone     = 0
	paddingVariant1 = 1

	firstPaddings  = 8
	maxPaddingSize = 255
	maxFrameSize   = 64 * 1024
	frameHeader    = 3

	headerPadding            = "padding"
	headerPaddingTypeRequest = "padding-type-request"
	headerPaddingTypeReply   = "padding-type-reply"
)

// paddingTypeRequest lists the types naive offers, preferred first.
const paddingTypeRequest = "1, 0"

// replyPadding decides the padding type from the CONNECT reply, or reports a reply naive would reject.
func replyPadding(get func(string) string) (int, bool) {
	if v := strings.TrimSpace(get(headerPaddingTypeReply)); v != "" {
		switch t, err := strconv.Atoi(v); {
		case err == nil && (t == paddingNone || t == paddingVariant1):
			return t, true
		default:
			return 0, false
		}
	}
	// Servers older than the padding-type headers answer a padded request with a padding header alone.
	if get(headerPadding) != "" {
		return paddingVariant1, true
	}
	return paddingNone, true
}

var (
	nonindexOnce  sync.Once
	nonindexCodes [17]byte
)

// paddingHeaderValue is the random-length padding header value: characters whose HPACK Huffman codes are at least
// 8 bits long, so the value does not compress, picked like naive's FillNonindexHeaderValue.
func paddingHeaderValue() string {
	nonindexOnce.Do(func() {
		i := 0
		for c := byte(0x20); c <= 0x7f && i < len(nonindexCodes); c++ {
			// Eight copies take exactly as many bytes as one copy takes bits.
			if hpack.HuffmanEncodeLength(strings.Repeat(string(c), 8)) >= 8 {
				nonindexCodes[i] = c
				i++
			}
		}
	})
	b := make([]byte, 16+mrand.IntN(17))
	var seed [8]byte
	_, _ = rand.Read(seed[:])
	bits := binary.LittleEndian.Uint64(seed[:])
	for i := range b {
		if i < 16 {
			b[i] = nonindexCodes[bits&0xf]
			bits >>= 4
		} else {
			b[i] = nonindexCodes[16]
		}
	}
	return string(b)
}

// paddedWriter frames the first writes of a tunnel.
type paddedWriter struct {
	w      io.Writer
	frames int
	buf    []byte
}

func (p *paddedWriter) Write(b []byte) (int, error) {
	n := 0
	for n < len(b) && p.frames < firstPaddings {
		pad := mrand.IntN(maxPaddingSize + 1)
		chunk := b[n:min(len(b), n+maxFrameSize-frameHeader-pad)]
		p.buf = append(p.buf[:0], byte(len(chunk)>>8), byte(len(chunk)), byte(pad))
		p.buf = append(p.buf, chunk...)
		p.buf = append(p.buf, make([]byte, pad)...)
		if _, err := p.w.Write(p.buf); err != nil {
			return n, err
		}
		p.frames++
		n += len(chunk)
	}
	if n == len(b) {
		return n, nil
	}
	m, err := p.w.Write(b[n:])
	return n + m, err
}

// paddedReader strips the frames from the first reads of a tunnel.
type paddedReader struct {
	r       *bufio.Reader
	frames  int
	payload int
	pad     int
	hdr     [frameHeader]byte
}

func newPaddedReader(r io.Reader) *paddedReader { return &paddedReader{r: bufio.NewReader(r)} }

func (p *paddedReader) Read(b []byte) (int, error) {
	for {
		if p.frames >= firstPaddings {
			return p.r.Read(b)
		}
		if p.payload == 0 {
			if _, err := io.ReadFull(p.r, p.hdr[:]); err != nil {
				return 0, err
			}
			p.payload = int(p.hdr[0])<<8 | int(p.hdr[1])
			p.pad = int(p.hdr[2])
			if p.payload == 0 {
				if err := p.endFrame(); err != nil {
					return 0, err
				}
				continue
			}
		}
		n, err := p.r.Read(b[:min(len(b), p.payload)])
		p.payload -= n
		if p.payload == 0 && err == nil {
			err = p.endFrame()
		}
		if err == io.EOF && p.payload > 0 {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
}

func (p *paddedReader) endFrame() error {
	if _, err := p.r.Discard(p.pad); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	p.frames++
	return nil
}
