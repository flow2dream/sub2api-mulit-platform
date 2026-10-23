package kiro

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"unicode/utf8"
)

const MaxFrameSize = 16 << 20

type Frame struct {
	Headers map[string]string
	Payload json.RawMessage
}

// ReadFrame verifies both CRCs and all lengths before interpreting any payload.
// io.ReadFull makes packet boundaries irrelevant to frame or UTF-8 boundaries.
func ReadFrame(r io.Reader) (*Frame, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:]) {
		return nil, fmt.Errorf("kiro: prelude CRC mismatch")
	}
	total, headersLen := binary.BigEndian.Uint32(prelude[:4]), binary.BigEndian.Uint32(prelude[4:8])
	if total < 16 || total > MaxFrameSize || headersLen > total-16 {
		return nil, fmt.Errorf("kiro: invalid frame length")
	}
	rest := make([]byte, int(total)-12)
	if _, err := io.ReadFull(r, rest); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	h := crc32.NewIEEE()
	_, _ = h.Write(prelude[:])
	_, _ = h.Write(rest[:len(rest)-4])
	if h.Sum32() != binary.BigEndian.Uint32(rest[len(rest)-4:]) {
		return nil, fmt.Errorf("kiro: message CRC mismatch")
	}
	headers, err := decodeHeaders(rest[:headersLen])
	if err != nil {
		return nil, err
	}
	payload := rest[headersLen : len(rest)-4]
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return nil, fmt.Errorf("kiro: invalid event JSON")
	}
	return &Frame{Headers: headers, Payload: payload}, nil
}

func decodeHeaders(b []byte) (map[string]string, error) {
	out := make(map[string]string)
	for len(b) > 0 {
		n := int(b[0])
		b = b[1:]
		if n == 0 || len(b) < n+1 {
			return nil, fmt.Errorf("kiro: truncated header")
		}
		key, typ := string(b[:n]), b[n]
		b = b[n+1:]
		size := 0
		switch typ {
		case 0, 1:
			size = 0
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7:
			if len(b) < 2 {
				return nil, fmt.Errorf("kiro: truncated header length")
			}
			size = int(binary.BigEndian.Uint16(b[:2]))
			b = b[2:]
		default:
			return nil, fmt.Errorf("kiro: unsupported header encoding")
		}
		if len(b) < size {
			return nil, fmt.Errorf("kiro: truncated header value")
		}
		if typ == 7 {
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("kiro: duplicate header")
			}
			out[key] = string(b[:size])
		}
		b = b[size:]
	}
	return out, nil
}
