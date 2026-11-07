// Package invstats summarizes saved stage-one inventories (the local JSONL
// copies that `dug list` keeps) without downloading anything: how many entries
// there are, how many bytes a full download would need where the registry
// published sizes, and which packages lead on popularity facts.
package invstats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TopN is how many packages each ranking keeps.
const TopN = 10

// Item is one ranked package. Value is the package's best figure across its
// versions, since popularity facts describe the whole package.
type Item struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Version   string `json:"version"`
	Value     int64  `json:"value"`
}

// Ranking is a top-N list for one statistic.
type Ranking struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Items []Item `json:"items"`
}

// Summary describes one inventory file.
type Summary struct {
	ID        string            `json:"id"`
	File      string            `json:"file"`
	Registry  string            `json:"registry"`
	Provider  string            `json:"provider,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Limit     int               `json:"limit,omitempty"`
	// Complete is true when the file ends with a completion record whose
	// count matches the entries read.
	Complete bool `json:"complete"`
	// Entries counts versions, which is what a download fetches. Packages
	// counts distinct names.
	Entries  int64 `json:"entries"`
	Packages int64 `json:"packages"`
	// SizedEntries have a registry-published archive size; UnsizedEntries do
	// not (WordPress, for one, publishes none). KnownBytes sums only the
	// sized ones, so it is a lower bound whenever UnsizedEntries is nonzero.
	SizedEntries   int64     `json:"sized_entries"`
	UnsizedEntries int64     `json:"unsized_entries"`
	KnownBytes     int64     `json:"known_bytes"`
	Rankings       []Ranking `json:"rankings"`
}

type fileLine struct {
	Type   string `json:"type"`
	Header *struct {
		Registry  string            `json:"registry"`
		Provider  string            `json:"provider"`
		Kind      string            `json:"kind"`
		Options   map[string]string `json:"options"`
		CreatedAt time.Time         `json:"created_at"`
		Limit     int               `json:"limit"`
	} `json:"header"`
	Reference *struct {
		Namespace  string            `json:"namespace"`
		Name       string            `json:"name"`
		Version    string            `json:"version"`
		Attributes map[string]string `json:"attributes"`
		Metadata   map[string]string `json:"metadata"`
		Facts      *struct {
			Downloads *struct {
				Total *int64 `json:"total"`
			} `json:"downloads"`
			Installs *int64 `json:"installs"`
		} `json:"facts"`
	} `json:"reference"`
	Count int64 `json:"count"`
}

type ranker struct {
	key, label string
	best       map[string]Item
}

func (r *ranker) offer(pkg string, item Item) {
	if cur, ok := r.best[pkg]; !ok || item.Value > cur.Value {
		r.best[pkg] = item
	}
}

func (r *ranker) result() Ranking {
	items := make([]Item, 0, len(r.best))
	for _, item := range r.best {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Value != items[j].Value {
			return items[i].Value > items[j].Value
		}
		return items[i].Name < items[j].Name
	})
	if len(items) > TopN {
		items = items[:TopN]
	}
	return Ranking{Key: r.key, Label: r.label, Items: items}
}

// Scan reads one inventory file in a single streaming pass.
func Scan(path string) (Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return Summary{}, err
	}
	defer f.Close()
	sum := Summary{File: filepath.Base(path), ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	downloads := &ranker{"downloads", "Most downloads", map[string]Item{}}
	installs := &ranker{"installs", "Most installs", map[string]Item{}}
	stars := &ranker{"stars", "Most stars", map[string]Item{}}
	packages := map[string]struct{}{}
	sawHeader, sawComplete := false, false
	var declared int64
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	for {
		var line fileLine
		if err := dec.Decode(&line); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return Summary{}, fmt.Errorf("%s: %w", sum.File, err)
		}
		switch line.Type {
		case "header":
			if line.Header == nil {
				return Summary{}, fmt.Errorf("%s: header record is empty", sum.File)
			}
			h := line.Header
			sum.Registry, sum.Provider, sum.Kind = h.Registry, h.Provider, h.Kind
			sum.Options, sum.CreatedAt, sum.Limit = h.Options, h.CreatedAt, h.Limit
			sawHeader = true
		case "entry":
			ref := line.Reference
			if ref == nil {
				continue
			}
			sum.Entries++
			pkg := ref.Namespace + "/" + ref.Name
			packages[pkg] = struct{}{}
			item := Item{Name: ref.Name, Namespace: ref.Namespace, Version: ref.Version}
			if size, err := strconv.ParseInt(ref.Attributes["size"], 10, 64); err == nil && size > 0 {
				sum.SizedEntries++
				sum.KnownBytes += size
			} else {
				sum.UnsizedEntries++
			}
			if ref.Facts != nil {
				if ref.Facts.Downloads != nil && ref.Facts.Downloads.Total != nil {
					item.Value = *ref.Facts.Downloads.Total
					downloads.offer(pkg, item)
				}
				if ref.Facts.Installs != nil {
					item.Value = *ref.Facts.Installs
					installs.offer(pkg, item)
				}
			}
			if n, err := strconv.ParseInt(ref.Metadata["stars"], 10, 64); err == nil {
				item.Value = n
				stars.offer(pkg, item)
			}
		case "complete":
			sawComplete, declared = true, line.Count
		}
	}
	if !sawHeader {
		return Summary{}, fmt.Errorf("%s: no header record", sum.File)
	}
	sum.Packages = int64(len(packages))
	sum.Complete = sawComplete && declared == sum.Entries
	sum.Rankings = []Ranking{}
	for _, r := range []*ranker{downloads, installs, stars} {
		if len(r.best) > 0 {
			sum.Rankings = append(sum.Rankings, r.result())
		}
	}
	return sum, nil
}

// Dir scans every *.jsonl in dir and reports only the newest inventory for each
// registry, newest first. Older runs of the same registry (including the
// duplicate copies an inventory can have under its ULID and its content digest)
// are dropped. A file that cannot be read is returned in skipped instead of failing
// the rest.
type Dir struct {
	Path string

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	size    int64
	modTime time.Time
	sum     Summary
}

// Load returns the summaries and a "file: reason" line per skipped file.
// Results are cached per file until its size or modification time changes.
func (d *Dir) Load() (sums []Summary, skipped []string, err error) {
	paths, err := filepath.Glob(filepath.Join(d.Path, "*.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(d.Path); err != nil {
		return nil, nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache == nil {
		d.cache = map[string]cached{}
	}
	live := map[string]bool{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			skipped = append(skipped, filepath.Base(p)+": "+err.Error())
			continue
		}
		live[p] = true
		c, ok := d.cache[p]
		if !ok || c.size != info.Size() || !c.modTime.Equal(info.ModTime()) {
			sum, err := Scan(p)
			if err != nil {
				skipped = append(skipped, err.Error())
				delete(d.cache, p)
				continue
			}
			c = cached{info.Size(), info.ModTime(), sum}
			d.cache[p] = c
		}
		sums = append(sums, c.sum)
	}
	for p := range d.cache {
		if !live[p] {
			delete(d.cache, p)
		}
	}
	sort.Slice(sums, func(i, j int) bool {
		if !sums[i].CreatedAt.Equal(sums[j].CreatedAt) {
			return sums[i].CreatedAt.After(sums[j].CreatedAt)
		}
		return sums[i].ID < sums[j].ID
	})
	latest := map[string]bool{}
	kept := sums[:0]
	for _, sum := range sums {
		if !latest[sum.Registry] {
			latest[sum.Registry] = true
			kept = append(kept, sum)
		}
	}
	return kept, skipped, nil
}
