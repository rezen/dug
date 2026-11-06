// Package layout decides where objects live in the bucket. Everything that
// belongs to one kind of artifact sits under <registry>/<type>/, for example
// wordpress/plugins/artifacts/akismet/5.3/ and wordpress/plugins/last_run/.
package layout

import (
	"fmt"
	"strings"

	"github.com/rezen/dug/sdk/registry"
)

// ManifestName is the completion record inside every artifact directory. A
// blob may not use it.
const ManifestName = "manifest.json"

// types maps a registry identity to the artifact type it serves. Add an entry
// when adding a source; unlisted registries use defaultType.
var types = map[string]string{
	"ansible-galaxy":   "collections",
	"chrome":           "extensions",
	"firefox":          "addons",
	"github":           "repos",
	"helm":             "charts",
	"n8n":              "nodes",
	"obsidian":         "plugins",
	"wordpress":        "plugins",
	"wordpress-themes": "themes",
}

const defaultType = "packages"

// Dir returns "<registry>/<type>" for a registry identity. The WordPress theme
// registry is filed under the wordpress product, not wordpress-themes.
func Dir(registryID string) (string, error) {
	product := registryID
	typ, ok := types[registryID]
	if !ok {
		typ = defaultType
	}
	if registryID == "wordpress-themes" {
		product = "wordpress"
	}
	product, err := Segment(product)
	if err != nil {
		return "", fmt.Errorf("registry %q: %w", registryID, err)
	}
	return product + "/" + typ, nil
}

// LastRunPrefix is the directory holding last-run pointers for a registry.
func LastRunPrefix(registryID string) (string, error) {
	dir, err := Dir(registryID)
	if err != nil {
		return "", err
	}
	return dir + "/last_run/", nil
}

// ArtifactDir returns <registry>/<type>/artifacts/<name>/<version> with no
// trailing slash. Names such as GitHub's owner/repo keep their slash as a
// directory boundary; every other character that could escape or confuse a key
// is percent-encoded.
func ArtifactDir(ref registry.Reference) (string, error) {
	dir, err := Dir(ref.Registry)
	if err != nil {
		return "", err
	}
	if ref.Name == "" || ref.Version == "" {
		return "", fmt.Errorf("artifact needs a name and a version")
	}
	parts := strings.Split(ref.Name, "/")
	for i, part := range parts {
		if parts[i], err = Segment(part); err != nil {
			return "", fmt.Errorf("name %q: %w", ref.Name, err)
		}
	}
	version, err := Segment(ref.Version)
	if err != nil {
		return "", fmt.Errorf("version %q: %w", ref.Version, err)
	}
	return dir + "/artifacts/" + strings.Join(parts, "/") + "/" + version, nil
}

// BlobName validates a blob file name for use as the last key segment.
func BlobName(name string) (string, error) {
	segment, err := Segment(name)
	if err != nil {
		return "", err
	}
	if segment == ManifestName {
		return "", fmt.Errorf("blob name %q is reserved", name)
	}
	return segment, nil
}

// Segment percent-encodes everything outside a conservative set, so "/" and
// control characters never create extra path levels, and rejects the names
// "", "." and ".." that would climb out of the directory.
func Segment(s string) (string, error) {
	if s == "" || s == "." || s == ".." {
		return "", fmt.Errorf("invalid path segment %q", s)
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-', c == '@', c == '+', c == '=', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String(), nil
}

const inventoryDir = "/inventories/"

// InventoryKey is the directory key of one inventory for a registry:
// <registry>/<type>/inventories/<ulid>. It holds part-NNNNNN.jsonl segments and
// a complete.json commit record. ULIDs sort by creation time.
func InventoryKey(registryID, ulid string) (string, error) {
	if !ValidULID(ulid) {
		return "", fmt.Errorf("invalid inventory id %q", ulid)
	}
	dir, err := Dir(registryID)
	if err != nil {
		return "", err
	}
	return dir + inventoryDir + ulid, nil
}

// ParseInventoryKey splits "<registry>/<type>/inventories/<ulid>" into
// "<registry>/<type>" and the ULID. It checks shape only; callers must confirm
// the directory matches the registry named in the inventory.
func ParseInventoryKey(key string) (dir, ulid string, ok bool) {
	i := strings.Index(key, inventoryDir)
	if i < 0 {
		return "", "", false
	}
	dir = key[:i]
	ulid = key[i+len(inventoryDir):]
	if !ValidULID(ulid) {
		return "", "", false
	}
	parts := strings.Split(dir, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	for _, p := range parts {
		if seg, err := Segment(p); err != nil || seg != p {
			return "", "", false
		}
	}
	return dir, ulid, true
}
