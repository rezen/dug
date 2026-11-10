// Package livetest checks a registry source against its real upstream so
// provider-side changes show up as failing contract assertions. Sources call
// Run from tests behind the "live" build tag; the default test run never
// touches the network.
package livetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services"
)

// Fact names accepted in Spec.Facts.
const (
	FactDownloadsTotal   = "downloads.total"
	FactDownloadsMonth   = "downloads.month"
	FactDownloadsWeek    = "downloads.week"
	FactVersionDownloads = "version_downloads.total"
	FactInstalls         = "installs"
	FactRepository       = "repository"
	FactReleasedAt       = "released_at"
	FactUpdatedAt        = "updated_at"
)

// Spec describes what one source must still deliver from its upstream.
type Spec struct {
	// Factory is the source's plugin factory, built with Client(); Options narrow discovery to a
	// small, stable slice of the catalog.
	Factory plugin.Factory
	Options map[string]string
	// MinRefs is the fewest references discovery must emit (default 1).
	// Discovery stops after MaxRefs (default 5).
	MinRefs, MaxRefs int
	// Facts lists the registry.Facts fields every emitted reference must carry.
	Facts []string
	// Metadata lists metadata keys every emitted reference must carry.
	Metadata []string
	// Tags, Author and Raw require those reference fields to be populated.
	Tags, Author, Raw bool
	// Digest requires every blob to pin a sha256 digest; Size requires a known size.
	Digest, Size bool
	// MaxDownload caps how much of a blob is read to verify it (default 8 MiB).
	// Larger blobs are only sampled, not hashed.
	MaxDownload int64
	// Ref and Artifact add source-specific assertions on the first reference
	// and its resolved artifact.
	Ref      func(*testing.T, registry.Reference)
	Artifact func(*testing.T, registry.Artifact)
}

// Client returns the HTTP client sources should be built with in live tests.
func Client() *http.Client { return services.NewDugHttpClient(services.WithConnections(4)) }

var errEnough = errors.New("livetest: enough references")

// Run discovers a few references, resolves the first, and downloads its first
// blob. Transient upstream trouble (timeouts, 5xx, 429) skips the test so only
// contract drift fails it.
func Run(t *testing.T, spec Spec) {
	t.Helper()
	if spec.MinRefs == 0 {
		spec.MinRefs = 1
	}
	if spec.MaxRefs == 0 {
		spec.MaxRefs = 5
	}
	if spec.MaxDownload == 0 {
		spec.MaxDownload = 8 << 20
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	reg, err := spec.Factory(spec.Options)
	if err != nil {
		t.Fatalf("factory rejected options %v: %v", spec.Options, err)
	}

	var refs []registry.Reference
	err = reg.Discover(ctx, func(ref registry.Reference) error {
		refs = append(refs, ref)
		if len(refs) >= spec.MaxRefs {
			return errEnough
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnough) {
		fail(t, "discover", err)
	}
	if len(refs) < spec.MinRefs {
		t.Fatalf("discover emitted %d references, want at least %d", len(refs), spec.MinRefs)
	}
	for _, ref := range refs {
		checkRef(t, spec, ref)
	}
	if os.Getenv("LIVETEST_DUMP") != "" {
		dump := refs[0]
		dump.Raw = nil
		data, _ := json.MarshalIndent(dump, "", "  ")
		t.Logf("first reference (set by LIVETEST_DUMP):\n%s", data)
	}
	if spec.Ref != nil {
		spec.Ref(t, refs[0])
	}

	art, err := reg.Resolve(ctx, refs[0])
	if err != nil {
		fail(t, "resolve "+label(refs[0]), err)
	}
	if art.Revision == "" {
		t.Errorf("resolve %s: empty revision", label(refs[0]))
	}
	if len(art.Blobs) == 0 {
		t.Fatalf("resolve %s: no blobs", label(refs[0]))
	}
	seen := map[string]bool{}
	for _, blob := range art.Blobs {
		if blob.Name == "" || seen[blob.Name] {
			t.Errorf("blob name %q is empty or duplicated", blob.Name)
		}
		seen[blob.Name] = true
		if blob.Open == nil {
			t.Fatalf("blob %q has no Open", blob.Name)
		}
		if blob.Digest != "" && !validDigest(blob.Digest) {
			t.Errorf("blob %q digest %q is not lowercase sha256:<hex>", blob.Name, blob.Digest)
		}
		if spec.Digest && blob.Digest == "" {
			t.Errorf("blob %q has no digest", blob.Name)
		}
		if spec.Size && blob.Size < 0 {
			t.Errorf("blob %q has no size", blob.Name)
		}
	}
	if os.Getenv("LIVETEST_DUMP") != "" {
		for _, b := range art.Blobs {
			t.Logf("blob %s type=%s size=%d digest=%s", b.Name, b.MediaType, b.Size, b.Digest)
		}
	}
	if spec.Artifact != nil {
		spec.Artifact(t, art)
	}
	download(ctx, t, art.Blobs[0], spec.MaxDownload)
}

func label(ref registry.Reference) string {
	return strings.Trim(ref.Namespace+"/"+ref.Name+"@"+ref.Version, "/")
}

func checkRef(t *testing.T, spec Spec, ref registry.Reference) {
	t.Helper()
	l := label(ref)
	if ref.Name == "" || ref.Version == "" {
		t.Errorf("reference %s lacks a name or version", l)
	}
	if spec.Tags && len(ref.Tags) == 0 {
		t.Errorf("%s: no tags", l)
	}
	if spec.Author && (ref.Author == nil || ref.Author.Name+ref.Author.Username == "") {
		t.Errorf("%s: no author", l)
	}
	if spec.Raw && len(ref.Raw) == 0 {
		t.Errorf("%s: no raw upstream record", l)
	}
	for _, key := range spec.Metadata {
		if ref.Metadata[key] == "" {
			t.Errorf("%s: metadata %q missing", l, key)
		}
	}
	for _, name := range spec.Facts {
		if !hasFact(ref.Facts, name) {
			t.Errorf("%s: fact %q missing", l, name)
		}
	}
}

func hasFact(f *registry.Facts, name string) bool {
	if f == nil {
		return false
	}
	switch name {
	case FactDownloadsTotal:
		return f.Downloads != nil && f.Downloads.Total != nil
	case FactDownloadsMonth:
		return f.Downloads != nil && f.Downloads.Month != nil
	case FactDownloadsWeek:
		return f.Downloads != nil && f.Downloads.Week != nil
	case FactVersionDownloads:
		return f.VersionDownloads != nil && f.VersionDownloads.Total != nil
	case FactInstalls:
		return f.Installs != nil
	case FactRepository:
		return f.Repository != nil && f.Repository.URL != ""
	case FactReleasedAt:
		return f.ReleasedAt != nil && !f.ReleasedAt.IsZero()
	case FactUpdatedAt:
		return f.UpdatedAt != nil && !f.UpdatedAt.IsZero()
	}
	panic("livetest: unknown fact " + name)
}

func validDigest(d string) bool {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hexPart) != 64 {
		return false
	}
	return hexPart == strings.ToLower(hexPart) && func() bool { _, err := hex.DecodeString(hexPart); return err == nil }()
}

// download reads the blob. A blob that fits MaxDownload is hashed and sized
// against what the source declared; a larger one is only sampled.
func download(ctx context.Context, t *testing.T, blob registry.Blob, max int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body, err := blob.Open(ctx)
	if err != nil {
		fail(t, "open blob "+blob.Name, err)
	}
	defer body.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(body, max+1))
	if err != nil {
		fail(t, "read blob "+blob.Name, err)
	}
	if n == 0 {
		t.Fatalf("blob %q downloaded empty", blob.Name)
	}
	if n > max {
		return
	}
	if blob.Size >= 0 && n != blob.Size {
		t.Errorf("blob %q: declared size %d, downloaded %d bytes", blob.Name, blob.Size, n)
	}
	if blob.Digest != "" {
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != blob.Digest {
			t.Errorf("blob %q: declared %s, downloaded %s", blob.Name, blob.Digest, got)
		}
	}
}

// fail skips for transient upstream trouble and fails for anything else.
func fail(t *testing.T, step string, err error) {
	t.Helper()
	if transient(err) {
		t.Skipf("%s: upstream unavailable: %v", step, err)
	}
	t.Fatalf("%s: %v", step, err)
}

func transient(err error) bool {
	var status *httpx.StatusError
	if errors.As(err, &status) {
		return status.Code == http.StatusTooManyRequests || status.Code >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF)
}

var _ = fmt.Sprint
