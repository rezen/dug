package github

import (
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

// Repo is a GitHub repository record. Raw keeps the verbatim API response.
type Repo struct {
	ID            int64     `json:"id"`
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	Fork          bool      `json:"fork"`
	Archived      bool      `json:"archived"`
	DefaultBranch string    `json:"default_branch"`
	HTMLURL       string    `json:"html_url"`
	Language      string    `json:"language"`
	Stars         int64     `json:"stargazers_count"`
	Forks         int64     `json:"forks_count"`
	OpenIssues    int64     `json:"open_issues_count"`
	CreatedAt     time.Time `json:"created_at"`
	PushedAt      time.Time `json:"pushed_at"`
	License       *struct {
		SPDX string `json:"spdx_id"`
	} `json:"license"`
	Topics []string `json:"topics"`
	Owner  struct {
		Login   string `json:"login"`
		ID      int64  `json:"id"`
		Type    string `json:"type"`
		HTMLURL string `json:"html_url"`
	} `json:"owner"`
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the verbatim repository record next to the decoded fields.
func (r *Repo) UnmarshalJSON(data []byte) error {
	type plain Repo
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	r.Raw = slices.Clone(data)
	return nil
}

// AuthorOf is the repository owner. GitHub exposes no display name on the
// repository object; the numeric ID is stable across login renames.
func (r Repo) AuthorOf() *registry.Author {
	if r.Owner.Login == "" {
		return nil
	}
	author := registry.Author{Username: r.Owner.Login, URL: r.Owner.HTMLURL}
	if r.Owner.ID > 0 {
		author.ID = strconv.FormatInt(r.Owner.ID, 10)
	}
	return &author
}

// Asset is the persisted descriptor of one release asset. Download URLs are
// derived from the repository, tag and asset ID at resolution, never persisted.
type Asset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Digest      string `json:"digest,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// Release is a GitHub release with its assets.
type Release struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Asset
		State     string `json:"state"`
		Downloads int64  `json:"download_count"`
	} `json:"assets"`
}
