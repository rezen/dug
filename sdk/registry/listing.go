package registry

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Author is the registry account that publishes the package. Like Facts it is
// a mutable listing fact excluded from artifact identity: a change of author
// between inventories is itself a supply-chain signal. Fill whatever the
// registry publishes and leave the rest empty; ID is the account identifier
// that survives renames, Username the human-facing handle.
type Author struct {
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
	ID       string `json:"id,omitempty"`
	URL      string `json:"url,omitempty"`
	Email    string `json:"email,omitempty"`
}

// Validate rejects an author with no identifying field at all.
func (a *Author) Validate() error {
	if a == nil {
		return nil
	}
	if a.Name == "" && a.Username == "" && a.ID == "" && a.URL == "" && a.Email == "" {
		return fmt.Errorf("author: no name, username, id, url or email")
	}
	return nil
}

// Clone copies the author so emitted references share nothing mutable.
func (a *Author) Clone() *Author {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}

// NormalizeTags trims, lowercases, de-duplicates and sorts registry tags,
// topics and category slugs so the same tag compares equal across registries
// and inventories encode deterministically. Empty tags are dropped; an empty
// result is nil so it is omitted from JSON.
func NormalizeTags(tags []string) []string {
	var out []string
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			out = append(out, tag)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ValidateTags rejects tags that NormalizeTags would not produce.
func ValidateTags(tags []string) error {
	for i, tag := range tags {
		if tag == "" || tag != strings.ToLower(strings.TrimSpace(tag)) {
			return fmt.Errorf("tag %q is not trimmed lowercase", tag)
		}
		if i > 0 && tags[i-1] >= tag {
			return fmt.Errorf("tags are not sorted and unique at %q", tag)
		}
	}
	return nil
}

// HasTag reports whether the reference carries tag, in normalized form.
func (r Reference) HasTag(tag string) bool {
	_, found := slices.BinarySearch(r.Tags, strings.ToLower(strings.TrimSpace(tag)))
	return found
}

// ValidateListing checks the mutable listing facts a registry attached to a
// reference: typed facts, tags, author and raw record. Hosts call it when persisting an
// inventory and on both sides of the plugin wire.
func (r Reference) ValidateListing() error {
	if err := r.Facts.Validate(); err != nil {
		return err
	}
	if err := ValidateTags(r.Tags); err != nil {
		return err
	}
	if len(r.Raw) > 0 && !json.Valid(r.Raw) {
		return fmt.Errorf("raw record is not valid JSON")
	}
	return r.Author.Validate()
}
