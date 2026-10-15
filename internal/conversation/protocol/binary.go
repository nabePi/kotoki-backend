package protocol

import (
	"encoding/binary"
	"errors"
)

// Frame biner audio (spec §7.4.7). Semua integer big-endian unsigned.
const (
	// MaxBinary adalah ukuran maksimum satu frame biner (header + payload).
	MaxBinary = 4096

	frameUpstream   byte = 0x01
	frameDownstream byte = 0x02

	upstreamHeader   = 5
	downstreamHeader = 9
)

// ErrFrameTooShort berarti frame lebih pendek dari header + 1 byte payload.
var ErrFrameTooShort = errors.New("protocol: binary frame too short")

// ErrFrameTooLarge berarti frame lebih dari MaxBinary byte.
var ErrFrameTooLarge = errors.New("protocol: binary frame too large")

// ErrBadType berarti byte 0 bukan tipe yang diharapkan arah tersebut.
var ErrBadType = errors.New("protocol: bad binary frame type")

// EncodeUpstream membangun frame client -> server: 0x01 | seq | opus.
func EncodeUpstream(seq uint32, opus []byte) []byte {
	b := make([]byte, upstreamHeader+len(opus))
	b[0] = frameUpstream
	binary.BigEndian.PutUint32(b[1:5], seq)
	copy(b[upstreamHeader:], opus)
	return b
}

// DecodeUpstream memvalidasi dan membaca frame client -> server. Slice opus
// berbagi memori dengan b. Semua error memetakan ke PROTOCOL_VIOLATION (1008).
func DecodeUpstream(b []byte) (seq uint32, opus []byte, err error) {
	if len(b) > MaxBinary {
		return 0, nil, ErrFrameTooLarge
	}
	if len(b) <= upstreamHeader {
		return 0, nil, ErrFrameTooShort
	}
	if b[0] != frameUpstream {
		return 0, nil, ErrBadType
	}
	return binary.BigEndian.Uint32(b[1:5]), b[upstreamHeader:], nil
}

// EncodeDownstream membangun frame server -> client: 0x02 | gen | seq | opus.
func EncodeDownstream(gen, seq uint32, opus []byte) []byte {
	b := make([]byte, downstreamHeader+len(opus))
	b[0] = frameDownstream
	binary.BigEndian.PutUint32(b[1:5], gen)
	binary.BigEndian.PutUint32(b[5:9], seq)
	copy(b[downstreamHeader:], opus)
	return b
}

// DecodeDownstream membaca frame server -> client (dipakai tes/client).
func DecodeDownstream(b []byte) (gen, seq uint32, opus []byte, err error) {
	if len(b) > MaxBinary {
		return 0, 0, nil, ErrFrameTooLarge
	}
	if len(b) <= downstreamHeader {
		return 0, 0, nil, ErrFrameTooShort
	}
	if b[0] != frameDownstream {
		return 0, 0, nil, ErrBadType
	}
	return binary.BigEndian.Uint32(b[1:5]), binary.BigEndian.Uint32(b[5:9]), b[downstreamHeader:], nil
}
