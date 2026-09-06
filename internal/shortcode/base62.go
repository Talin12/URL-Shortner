// Package shortcode converts numeric link IDs to and from the short strings
// that appear in URLs.
//
// Phase 1 encodes the ID directly, which means codes are sequential and
// therefore enumerable: whoever holds /aB3 can walk to /aB4. That is a known
// and deliberate property of the baseline; PLAN.md section 5.4 replaces it
// with a Feistel bijection once the block allocator lands.
package shortcode

import (
	"errors"
	"math"
	"strings"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const base = uint64(len(alphabet))

// ErrInvalidCode is returned for a string holding a character outside the
// base62 alphabet, or one long enough to overflow a uint64.
var ErrInvalidCode = errors.New("shortcode: invalid code")

// decodeTable maps a byte back to its digit value, or -1 when the byte is not
// part of the alphabet. Built once so Decode stays allocation-free.
var decodeTable = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	return t
}()

// Encode renders id in base62. Zero encodes as "0".
func Encode(id uint64) string {
	if id == 0 {
		return string(alphabet[0])
	}

	// 11 base62 digits cover the whole uint64 range.
	var buf [11]byte
	pos := len(buf)
	for id > 0 {
		pos--
		buf[pos] = alphabet[id%base]
		id /= base
	}
	return string(buf[pos:])
}

// Decode reverses Encode.
func Decode(code string) (uint64, error) {
	if code == "" || len(code) > 11 {
		return 0, ErrInvalidCode
	}

	var id uint64
	for i := 0; i < len(code); i++ {
		digit := decodeTable[code[i]]
		if digit < 0 {
			return 0, ErrInvalidCode
		}
		// Reject before multiplying rather than trying to detect the wrap
		// afterwards, which misses overflows that land back above id.
		if id > (math.MaxUint64-uint64(digit))/base {
			return 0, ErrInvalidCode
		}
		id = id*base + uint64(digit)
	}
	return id, nil
}

// Valid reports whether code contains only alphabet characters. Cheaper than
// Decode when the caller only needs to reject junk before touching the store.
func Valid(code string) bool {
	if code == "" || len(code) > 11 {
		return false
	}
	return strings.IndexFunc(code, func(r rune) bool {
		return r > 255 || decodeTable[byte(r)] < 0
	}) == -1
}
