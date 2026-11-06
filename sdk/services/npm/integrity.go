package npm

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"slices"
	"strings"
	"time"
)

// Integrity is the content hash npm publishes for a tarball: SHA-512 for
// every version published since 2017, SHA-1 for older ones. It is not SHA-256,
// so sources leave Blob.Digest empty, let the host compute SHA-256, and use
// Verify to hold the registry to its own hash while the bytes stream.
type Integrity struct {
	Algorithm string // "sha512" or "sha1"
	Sum       []byte
}

var sizes = map[string]int{"sha512": sha512.Size, "sha1": sha1.Size}

// ParseIntegrity accepts exactly one subresource-integrity token,
// sha512-BASE64 or sha1-BASE64, in canonical padded form. Lists of hashes and
// other algorithms are refused: a persisted value must mean one thing.
func ParseIntegrity(value string) (Integrity, error) {
	algorithm, encoded, ok := strings.Cut(value, "-")
	size, known := sizes[algorithm]
	if !ok || !known {
		return Integrity{}, fmt.Errorf("invalid npm integrity %q", value)
	}
	sum, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(sum) != size || base64.StdEncoding.EncodeToString(sum) != encoded {
		return Integrity{}, fmt.Errorf("invalid npm integrity %q", value)
	}
	return Integrity{Algorithm: algorithm, Sum: sum}, nil
}

// FromShasum converts the legacy dist.shasum, 40 lowercase hex digits of
// SHA-1, for versions published without an integrity field.
func FromShasum(shasum string) (Integrity, error) {
	sum, err := hex.DecodeString(shasum)
	if err != nil || len(sum) != sha1.Size || hex.EncodeToString(sum) != shasum {
		return Integrity{}, fmt.Errorf("invalid npm shasum %q", shasum)
	}
	return Integrity{Algorithm: "sha1", Sum: sum}, nil
}

// String is the canonical form ParseIntegrity reads back.
func (i Integrity) String() string {
	return i.Algorithm + "-" + base64.StdEncoding.EncodeToString(i.Sum)
}

func (i Integrity) hasher() hash.Hash {
	if i.Algorithm == "sha1" {
		return sha1.New()
	}
	return sha512.New()
}

// Verify wraps a tarball body so that reading it to EOF fails with an error
// unless the bytes hash to the integrity value. A reader that stops early is
// not verified; the host always reads blobs to the end to compute its digest.
func (i Integrity) Verify(body io.ReadCloser) io.ReadCloser {
	return &verifier{ReadCloser: body, want: i, sum: i.hasher()}
}

type verifier struct {
	io.ReadCloser
	want Integrity
	sum  hash.Hash
}

func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.ReadCloser.Read(p)
	v.sum.Write(p[:n])
	if err == io.EOF && !bytes.Equal(v.sum.Sum(nil), v.want.Sum) {
		return n, fmt.Errorf("npm tarball does not match its published %s integrity", v.want.Algorithm)
	}
	return n, err
}

// sortVersions orders versions newest first by publish time; versions without
// a time sort last, and ties fall back to the version string descending so the
// result never depends on map order.
func sortVersions(versions []string, published func(string) time.Time) {
	slices.SortFunc(versions, func(a, b string) int {
		ta, tb := published(a), published(b)
		switch {
		case ta.IsZero() != tb.IsZero():
			if ta.IsZero() {
				return 1
			}
			return -1
		case !ta.Equal(tb):
			if ta.After(tb) {
				return -1
			}
			return 1
		}
		return strings.Compare(b, a)
	})
}
