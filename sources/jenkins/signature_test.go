package jenkins

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

// pki is a throwaway root and the leaf it issued, standing in for Jenkins'.
type pki struct {
	pool    *x509.CertPool
	leaf    *x509.Certificate
	leafKey *rsa.PrivateKey
}

func newPKI(t testing.TB, notAfter time.Time) *pki {
	t.Helper()
	issue := func(subject string, template *x509.Certificate, parent *x509.Certificate, parentKey, key *rsa.PrivateKey) *x509.Certificate {
		template.SerialNumber = big.NewInt(time.Now().UnixNano())
		template.Subject = pkix.Name{CommonName: subject}
		template.NotBefore = time.Now().Add(-time.Hour)
		template.NotAfter = notAfter
		if parent == nil {
			parent, parentKey = template, key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	root := issue("test root", &x509.Certificate{IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}, nil, nil, rootKey)
	leaf := issue("test update center", &x509.Certificate{}, root, rootKey, leafKey)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return &pki{pool: pool, leaf: leaf, leafKey: leafKey}
}

var trusted = sync.OnceValue(func() *pki { return newPKI(new(testing.T), time.Now().Add(24*time.Hour)) })

// sign returns body with a signature member appended. It does not use the
// package's canonicalizer: the signed bytes are json.Compact of body, so a
// document written in its canonical form verifies only if canonicalize agrees
// with that independent reading of the scheme. A body that is not JSON is
// returned unsigned.
func sign(t testing.TB, p *pki, body string) string {
	t.Helper()
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(body)) != nil {
		return body
	}
	sum := sha512.Sum512(compact.Bytes())
	raw, err := rsa.SignPKCS1v15(rand.Reader, p.leafKey, crypto.SHA512, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	member := fmt.Sprintf(`"signature":{"certificates":[%q],"correct_digest512":%q,"correct_signature512":%q}`,
		base64.StdEncoding.EncodeToString(p.leaf.Raw), hex.EncodeToString(sum[:]), hex.EncodeToString(raw))
	return strings.TrimSuffix(compact.String(), "}") + "," + member + "}"
}

func writeSigned(t testing.TB, w io.Writer, format string, args ...any) {
	io.WriteString(w, sign(t, trusted(), fmt.Sprintf(format, args...)))
}

func TestVerifyDocument(t *testing.T) {
	p := trusted()
	now := time.Now()
	// Raw UTF-8, short escapes and a control character are written the way
	// the signer writes them; a key order that is not alphabetical is kept.
	body := `{"zeta":1,"alpha":[true,null,1.50,"café \"q\" \\ \n \u0001 </a>"],"nested":{"b":"x","a":"y"}}`
	body = strings.Replace(body, `é`, "é", 1)
	good := sign(t, p, body)
	if err := verifyDocument([]byte(good), p.pool, now); err != nil {
		t.Fatal(err)
	}
	other := newPKI(t, now.Add(24*time.Hour))
	for name, tc := range map[string]struct {
		data  string
		roots *x509.CertPool
		now   time.Time
	}{
		"changed value":         {strings.Replace(good, `"x"`, `"y"`, 1), p.pool, now},
		"added member":          {strings.Replace(good, `{"zeta":1,`, `{"extra":1,"zeta":1,`, 1), p.pool, now},
		"reordered members":     {strings.Replace(good, `{"zeta":1,"alpha"`, `{"alpha"`, 1), p.pool, now},
		"unsigned":              {body, p.pool, now},
		"untrusted root":        {good, other.pool, now},
		"expired signing cert":  {good, p.pool, now.Add(48 * time.Hour)},
		"signed by another key": {sign(t, &pki{pool: p.pool, leaf: p.leaf, leafKey: other.leafKey}, body), p.pool, now},
		"two signatures":        {strings.TrimSuffix(good, "}") + `,"signature":{}}`, p.pool, now},
		"trailing data":         {good + `{}`, p.pool, now},
		"not an object":         {`[1]`, p.pool, now},
	} {
		if err := verifyDocument([]byte(tc.data), tc.roots, tc.now); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	// Whitespace outside strings is not signed, so a reformatted copy verifies.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(good), "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := verifyDocument(pretty.Bytes(), p.pool, now); err != nil {
		t.Errorf("reformatted copy: %v", err)
	}
	// A signature copied from another document does not transfer.
	donor := sign(t, p, `{"other":1}`)
	spliced := good[:strings.Index(good, `"signature"`)] + donor[strings.Index(donor, `"signature"`):]
	if err := verifyDocument([]byte(spliced), p.pool, now); err == nil {
		t.Error("spliced signature verified")
	}
}

func TestDiscoverRejectsUnsignedAndTamperedCatalogs(t *testing.T) {
	good := `{"plugins":{"a":{"name":"a","version":"1","url":"%s/download/plugins/a/1/a.hpi","sha256":"` + b64("x") + `","releaseTimestamp":"2026-01-01T00:00:00.00Z"}}}`
	for name, tc := range map[string]struct {
		serve func(host string) string
		want  string
	}{
		"unsigned": {func(host string) string { return strings.ReplaceAll(good, "%s", host) }, "no SHA-512 signature"},
		"tampered after signing": {func(host string) string {
			return strings.Replace(sign(t, trusted(), strings.ReplaceAll(good, "%s", host)), `"version":"1"`, `"version":"2"`, 1)
		}, "signed digest"},
		"untrusted signer": {func(host string) string {
			return sign(t, newPKI(t, time.Now().Add(time.Hour)), strings.ReplaceAll(good, "%s", host))
		}, "not trusted"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.serve("http://"+r.Host))
			}))
			defer server.Close()
			err := source(server).Discover(context.Background(), func(registry.Reference) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	// Unlike a decode failure, a bad signature is retryable: a mirror can
	// briefly serve a stale or partial copy.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"plugins":{}}`) }))
	defer server.Close()
	if err := source(server).Discover(context.Background(), func(registry.Reference) error { return nil }); err == nil || strings.Contains(err.Error(), "permanent") {
		t.Fatalf("got %v", err)
	}
}

// TestEmbeddedRoots pins the roots to the fingerprints in roots/README.md, so
// a changed file is noticed, and checks each is a CA that is currently valid.
func TestEmbeddedRoots(t *testing.T) {
	want := map[string]string{
		"jenkins-update-center-root-ca-2":    "2ED6FE80E57AF16A2A2626E8A8235A6D1342E48CDF634A1CFABCE9341545E5B9",
		"jenkins-update-center-root-ca-2026": "492270D33F4DFBF1122857F9434058F52A5A11993DBCD5D013FE017CE657EE7C",
	}
	entries, err := rootFiles.ReadDir("roots")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".pem")
		fingerprint, ok := want[name]
		if !ok {
			continue // roots/README.md
		}
		seen++
		data, _ := rootFiles.ReadFile("roots/" + entry.Name())
		block, _ := pem.Decode(data)
		if block == nil {
			t.Fatalf("%s: not PEM", name)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(cert.Raw); strings.ToUpper(hex.EncodeToString(sum[:])) != fingerprint {
			t.Errorf("%s fingerprint %X, want %s", name, sum, fingerprint)
		}
		if !cert.IsCA || time.Now().After(cert.NotAfter) {
			t.Errorf("%s is not a valid CA now", name)
		}
	}
	if seen != len(want) {
		t.Fatalf("found %d of %d roots", seen, len(want))
	}
	if _, err := defaultRoots(); err != nil {
		t.Fatal(err)
	}
}
