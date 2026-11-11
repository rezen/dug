package jenkins

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
	"unicode/utf8"
)

// The update center signs each document the way Jenkins core verifies it: the
// document without its top-level "signature" member, written compactly with
// keys in document order, is hashed with SHA-512 and signed with the RSA key of
// a certificate that chains to one of Jenkins' update center roots. The
// SHA-1 digest and signature the member also carries are legacy and ignored.

//go:embed roots/jenkins-update-center-root-ca-2.pem roots/jenkins-update-center-root-ca-2026.pem
var rootFiles embed.FS

// defaultRoots returns the embedded Jenkins update center root certificates.
func defaultRoots() (*x509.CertPool, error) {
	entries, err := rootFiles.ReadDir("roots")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for _, entry := range entries {
		data, err := rootFiles.ReadFile("roots/" + entry.Name())
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("%s: no certificate in embedded root %s", Identity, entry.Name())
		}
	}
	return pool, nil
}

// signature is the document's "signature" member.
type signature struct {
	Certificates []string `json:"certificates"` // base64 DER, leaf first
	Digest512    string   `json:"correct_digest512"`
	Signature512 string   `json:"correct_signature512"`
}

// verifyDocument checks that data is a document the Jenkins update center
// signed. It runs before any of the document is interpreted, and fails closed:
// a missing signature, an untrusted or expired chain, or any altered byte is an
// error.
func verifyDocument(data []byte, roots *x509.CertPool, now time.Time) error {
	hash := sha512.New()
	w := bufio.NewWriterSize(hash, 64<<10)
	sig, err := canonicalize(w, data)
	if err != nil {
		return fmt.Errorf("%s signature: %w", Identity, err)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if sig == nil || len(sig.Certificates) == 0 || sig.Digest512 == "" || sig.Signature512 == "" {
		return fmt.Errorf("%s document carries no SHA-512 signature", Identity)
	}
	certs := make([]*x509.Certificate, 0, len(sig.Certificates))
	for _, encoded := range sig.Certificates {
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("%s signature certificate: %w", Identity, err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("%s signature certificate: %w", Identity, err)
		}
		certs = append(certs, cert)
	}
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return fmt.Errorf("%s signing certificate is not trusted: %w", Identity, err)
	}
	key, ok := certs[0].PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("%s signing key is not RSA", Identity)
	}
	digest := hash.Sum(nil)
	if claimed, err := hex.DecodeString(sig.Digest512); err != nil || !bytes.Equal(claimed, digest) {
		return fmt.Errorf("%s document does not match its signed digest", Identity)
	}
	raw, err := hex.DecodeString(sig.Signature512)
	if err != nil {
		return fmt.Errorf("%s signature: %w", Identity, err)
	}
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA512, digest, raw); err != nil {
		return fmt.Errorf("%s document signature is invalid: %w", Identity, err)
	}
	return nil
}

const maxJSONDepth = 64

// canonicalize writes the signed form of a JSON object document to w: compact,
// keys in document order, without the top-level "signature" member, which is
// returned. Every other byte of content, including duplicate keys, reaches the
// hash, so the later Unmarshal cannot see anything the signature did not cover.
func canonicalize(w *bufio.Writer, data []byte) (*signature, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("document is not a JSON object")
	}
	var sig *signature
	w.WriteByte('{')
	first := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := keyTok.(string)
		if key == "signature" {
			if sig != nil {
				return nil, fmt.Errorf("document has two signature members")
			}
			sig = new(signature)
			if err := dec.Decode(sig); err != nil {
				return nil, err
			}
			continue
		}
		if !first {
			w.WriteByte(',')
		}
		first = false
		writeString(w, key)
		w.WriteByte(':')
		if err := writeValue(dec, w, 1); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the document")
	}
	w.WriteByte('}')
	return sig, nil
}

func writeValue(dec *json.Decoder, w *bufio.Writer, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("document nests deeper than %d", maxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		closer := byte('}')
		if t == '[' {
			closer = ']'
		}
		w.WriteByte(byte(t))
		for first := true; dec.More(); first = false {
			if !first {
				w.WriteByte(',')
			}
			if t == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				writeString(w, key.(string))
				w.WriteByte(':')
			}
			if err := writeValue(dec, w, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		w.WriteByte(closer)
	case string:
		writeString(w, t)
	case json.Number:
		w.WriteString(string(t))
	case bool:
		w.WriteString(strconv.FormatBool(t))
	case nil:
		w.WriteString("null")
	}
	return nil
}

// writeString writes s the way the signer does: UTF-8 left as is, only quotes,
// backslashes and control characters escaped.
func writeString(w *bufio.Writer, s string) {
	w.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			w.WriteString(`\"`)
		case r == '\\':
			w.WriteString(`\\`)
		case r == '\n':
			w.WriteString(`\n`)
		case r == '\r':
			w.WriteString(`\r`)
		case r == '\t':
			w.WriteString(`\t`)
		case r == '\b':
			w.WriteString(`\b`)
		case r == '\f':
			w.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(w, `\u%04x`, r)
		default:
			var buf [utf8.UTFMax]byte
			w.Write(buf[:utf8.EncodeRune(buf[:], r)])
		}
	}
	w.WriteByte('"')
}
