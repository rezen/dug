package layout

import (
	"crypto/rand"
	"fmt"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a 26-character ULID: a 48-bit millisecond timestamp followed
// by 80 random bits, Crockford base32. Lexicographic order is creation order,
// so a bucket listing of inventories is sorted by time.
func NewULID(t time.Time) (string, error) {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("ulid randomness: %w", err)
	}
	var out [26]byte
	// 128 bits as 26 groups of 5, most significant first (the top group is 3 bits).
	for i := 0; i < 26; i++ {
		shift := 125 - 5*i
		var v byte
		for bit := 4; bit >= 0; bit-- {
			pos := shift + bit
			if pos < 0 || pos > 127 {
				continue
			}
			byteIdx, bitIdx := 15-pos/8, pos%8
			v |= (b[byteIdx] >> bitIdx & 1) << bit
		}
		out[i] = crockford[v]
	}
	return string(out[:]), nil
}

// ValidULID reports whether s is a canonical uppercase ULID.
func ValidULID(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := false
		for j := 0; j < len(crockford); j++ {
			if crockford[j] == c {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
