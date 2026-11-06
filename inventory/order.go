package inventory

import (
	"bufio"
	"cmp"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rezen/dug/sdk/registry"
)

// Order defines host-side scheduling independently of a registry plugin.
// Use ParseOrder; a zero Order preserves discovery order.
type Order struct {
	field               string // "", "name", "metadata:KEY" or a typed fact path
	fact                registry.FactKind
	numeric, descending bool
}

// orderAliases name the common typed facts and the rating metadata, all
// descending so the biggest or newest comes first.
var orderAliases = map[string]string{"updated": "-updated_at", "popular": "-installs", "downloads": "-downloads.total", "released": "-released_at", "rating": "-number:rating"}

// ParseOrder accepts discovery, name, a typed fact path such as downloads.week
// or released_at, metadata:KEY, or number:KEY. Prefix '-' reverses the order.
// updated, popular, downloads, released and rating are aliases for descending
// updated_at, installs, downloads.total, released_at and number:rating.
// Missing values always sort last; ties keep the original discovery order.
// Numbers are finite float64 values; dates compare chronologically.
func ParseOrder(spec string) (Order, error) {
	if alias, ok := orderAliases[spec]; ok {
		spec = alias
	}
	order := Order{descending: strings.HasPrefix(spec, "-")}
	spec = strings.TrimPrefix(spec, "-")
	switch {
	case spec == "discovery":
	case spec == "name":
		order.field = "name"
	case registry.FactKindOf(spec) != 0:
		order.field = spec
		order.fact = registry.FactKindOf(spec)
		order.numeric = order.fact != registry.FactString
	case strings.HasPrefix(spec, "metadata:") && len(spec) > len("metadata:"):
		order.field = spec
	case strings.HasPrefix(spec, "number:") && len(spec) > len("number:"):
		order.field = "metadata:" + strings.TrimPrefix(spec, "number:")
		order.numeric = true
	default:
		return Order{}, fmt.Errorf("invalid order %q: use discovery, name, updated, popular, downloads, rating, released, a fact path such as downloads.week, metadata:KEY or number:KEY (optional '-' prefix)", spec)
	}
	return order, nil
}

// streams reports whether entries can be written straight through in input
// order, skipping sort runs and merge passes entirely.
func (o Order) streams() bool { return o.field == "" && !o.descending }

type sortItem struct {
	Reference registry.Reference `json:"reference"`
	Sequence  int64              `json:"sequence"`
	Key       string             `json:"key,omitempty"`
	Number    float64            `json:"number,omitempty"`
	Missing   bool               `json:"missing,omitempty"`
}

func (o Order) item(ref registry.Reference, sequence int64) (sortItem, error) {
	item := sortItem{Reference: ref, Sequence: sequence}
	switch {
	case o.field == "name":
		item.Key = ref.Name
	case o.fact != 0:
		fact, ok := ref.Fact(o.field)
		item.Missing = !ok
		switch fact.Kind {
		case registry.FactNumber:
			item.Number = fact.Number
		case registry.FactDate:
			item.Number = float64(fact.Date.UnixMicro())
		case registry.FactString:
			item.Key = fact.String
		}
	case strings.HasPrefix(o.field, "metadata:"):
		item.Key = ref.Metadata[strings.TrimPrefix(o.field, "metadata:")]
		item.Missing = item.Key == ""
		if o.numeric && !item.Missing {
			value, err := strconv.ParseFloat(item.Key, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return item, fmt.Errorf("%s: metadata sort value %q is not a finite number", ref.Name, item.Key)
			}
			item.Number = value
		}
	}
	return item, nil
}
func (o Order) less(a, b sortItem) bool {
	if a.Missing != b.Missing {
		return !a.Missing
	}
	var comparison int
	switch {
	case o.field == "":
		comparison = cmp.Compare(a.Sequence, b.Sequence)
	case o.numeric:
		comparison = cmp.Compare(a.Number, b.Number)
	default:
		comparison = strings.Compare(a.Key, b.Key)
	}
	if comparison == 0 {
		return a.Sequence < b.Sequence
	}
	if o.descending {
		return comparison > 0
	}
	return comparison < 0
}

type sorter struct {
	dir                string
	order              Order
	limit, bytes, next int
	items              []sortItem
	runs               []string
	// Discovery order streams to a single output instead of sort runs.
	stream  *os.File
	buffer  *bufio.Writer
	encoder *json.Encoder
}

func newSorter(dir string, order Order, limit int) *sorter {
	return &sorter{dir: dir, order: order, limit: limit}
}
func (s *sorter) add(ctx context.Context, ref registry.Reference, sequence int64, size int) error {
	item, err := s.order.item(ref, sequence)
	if err != nil {
		return err
	}
	if s.order.streams() {
		if s.stream == nil {
			file, err := os.Create(s.nextPath())
			if err != nil {
				return err
			}
			s.stream, s.buffer = file, bufio.NewWriter(file)
			s.encoder = json.NewEncoder(s.buffer)
		}
		return s.encoder.Encode(item)
	}
	s.items = append(s.items, item)
	s.bytes += size
	if s.bytes >= s.limit {
		return s.flush(ctx)
	}
	return nil
}

// close releases the streaming output when a failed load abandons it midway.
func (s *sorter) close() error {
	if s.stream == nil {
		return nil
	}
	file := s.stream
	s.stream, s.buffer, s.encoder = nil, nil, nil
	return file.Close()
}
func (s *sorter) nextPath() string {
	s.next++
	return filepath.Join(s.dir, fmt.Sprintf("run-%08d.jsonl", s.next))
}
func (s *sorter) flush(ctx context.Context) error {
	if len(s.items) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.SliceStable(s.items, func(i, j int) bool { return s.order.less(s.items[i], s.items[j]) })
	path := s.nextPath()
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := bufio.NewWriter(file)
	encoder := json.NewEncoder(buffer)
	for _, item := range s.items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := encoder.Encode(item); err != nil {
			return err
		}
	}
	if err := buffer.Flush(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	s.runs = append(s.runs, path)
	s.items = nil
	s.bytes = 0
	return nil
}
func (s *sorter) finish(ctx context.Context) (string, error) {
	if s.stream != nil {
		path := s.stream.Name()
		if err := s.buffer.Flush(); err != nil {
			return "", err
		}
		return path, s.close()
	}
	if err := s.flush(ctx); err != nil {
		return "", err
	}
	if len(s.runs) == 0 {
		path := s.nextPath()
		file, err := os.Create(path)
		if err != nil {
			return "", err
		}
		return path, file.Close()
	}
	// Limit fan-in to bound open file descriptors for very large registries.
	for len(s.runs) > 1 {
		var next []string
		for start := 0; start < len(s.runs); start += 32 {
			batch := s.runs[start:min(start+32, len(s.runs))]
			path := s.nextPath()
			if err := merge(ctx, batch, path, s.order); err != nil {
				return "", err
			}
			for _, old := range batch {
				if err := os.Remove(old); err != nil {
					return "", err
				}
			}
			next = append(next, path)
		}
		s.runs = next
	}
	return s.runs[0], nil
}

type mergeItem struct {
	item sortItem
	run  int
}
type mergeHeap struct {
	values []mergeItem
	order  Order
}

func (h mergeHeap) Len() int           { return len(h.values) }
func (h mergeHeap) Less(i, j int) bool { return h.order.less(h.values[i].item, h.values[j].item) }
func (h mergeHeap) Swap(i, j int)      { h.values[i], h.values[j] = h.values[j], h.values[i] }
func (h *mergeHeap) Push(value any)    { h.values = append(h.values, value.(mergeItem)) }
func (h *mergeHeap) Pop() any {
	last := len(h.values) - 1
	value := h.values[last]
	h.values = h.values[:last]
	return value
}
func merge(ctx context.Context, paths []string, output string, order Order) error {
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	defer out.Close()
	buffer := bufio.NewWriter(out)
	encoder := json.NewEncoder(buffer)
	decoders := make([]*json.Decoder, len(paths))
	queue := &mergeHeap{order: order}
	for i, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		decoders[i] = json.NewDecoder(bufio.NewReader(file))
		var item sortItem
		if err := decoders[i].Decode(&item); errors.Is(err, io.EOF) {
			continue
		} else if err != nil {
			return err
		}
		heap.Push(queue, mergeItem{item: item, run: i})
	}
	for queue.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		value := heap.Pop(queue).(mergeItem)
		if err := encoder.Encode(value.item); err != nil {
			return err
		}
		var next sortItem
		if err := decoders[value.run].Decode(&next); errors.Is(err, io.EOF) {
			continue
		} else if err != nil {
			return err
		}
		heap.Push(queue, mergeItem{item: next, run: value.run})
	}
	if err := buffer.Flush(); err != nil {
		return err
	}
	return out.Close()
}
