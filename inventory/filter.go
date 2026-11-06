package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

// Filter is a conjunction of comparisons. Construct with ParseFilter.
// Missing facts and metadata never match, including !=. The zero value
// matches all.
type Filter struct{ terms []comparison }

// comparison kinds: fact compares a typed fact path by its own kind; tag tests
// tag membership; identity compares a reference field; number, date and
// metadata compare a metadata value numerically, chronologically or lexically.
type comparison struct {
	field, kind, op, value string
	fact                   registry.FactKind
	number                 float64
	date                   time.Time
}

// filterAliases map short names to typed fact paths and metadata keys.
var filterAliases = map[string]string{"downloads": "downloads.total", "installs": "installs", "active_installs": "installs", "updated": "updated_at", "released": "released_at", "repository": "repository.url", "author": "author.username", "ratings": "num_ratings"}

var andPattern = regexp.MustCompile(`(?i)\s+AND\s+`)
var comparisonPattern = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_.:-]*)\s*(>=|<=|!=|==|=|>|<)\s*(.+)$`)

func ParseFilter(spec string) (Filter, error) {
	f := Filter{}
	if strings.TrimSpace(spec) == "" {
		return f, nil
	}
	for _, part := range andPattern.Split(strings.TrimSpace(spec), -1) {
		m := comparisonPattern.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return f, fmt.Errorf("invalid filter %q: use FIELD OP VALUE joined by AND", part)
		}
		c := comparison{field: m[1], op: m[2], value: strings.TrimSpace(m[3]), kind: "number"}
		if strings.HasPrefix(c.value, `"`) {
			v, err := strconv.Unquote(c.value)
			if err != nil {
				return f, err
			}
			c.value = v
		}
		if alias, ok := filterAliases[c.field]; ok {
			c.field = alias
		}
		switch {
		case registry.FactKindOf(c.field) != 0:
			c.kind = "fact"
			c.fact = registry.FactKindOf(c.field)
		case c.field == "tag":
			if c.op != "=" && c.op != "==" && c.op != "!=" {
				return f, fmt.Errorf("tag %s: tags support = and != only", c.op)
			}
			c.kind = "tag"
		case c.field == "rating" || c.field == "num_ratings":
		case c.field == "name" || c.field == "version" || c.field == "registry" || c.field == "namespace":
			c.kind = "identity"
		default:
			prefix, key, ok := strings.Cut(c.field, ":")
			if !ok || key == "" || (prefix != "metadata" && prefix != "number" && prefix != "date") {
				return f, fmt.Errorf("unknown filter field %q: use a fact path such as downloads.total, released_at, repository.url or author.username, tag, an identity field, or metadata:KEY, number:KEY, date:KEY", c.field)
			}
			c.field = key
			c.kind = prefix
		}
		switch {
		case c.kind == "number" || c.fact == registry.FactNumber:
			var err error
			c.number, err = finiteNumber(c.value)
			if err != nil {
				return f, err
			}
		case c.kind == "date" || c.fact == registry.FactDate:
			var err error
			c.date, err = parseDate(c.value)
			if err != nil {
				return f, err
			}
		}
		f.terms = append(f.terms, c)
	}
	return f, nil
}

func finiteNumber(value string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid finite number %q", value)
	}
	return n, nil
}
func parseDate(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q: use YYYY-MM-DD or RFC3339", value)
}

func (f Filter) Match(ref registry.Reference) (bool, error) {
	for _, c := range f.terms {
		result, ok, err := c.compare(ref)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		matches := false
		switch c.op {
		case "=", "==":
			matches = result == 0
		case "!=":
			matches = result != 0
		case ">":
			matches = result > 0
		case ">=":
			matches = result >= 0
		case "<":
			matches = result < 0
		case "<=":
			matches = result <= 0
		}
		if !matches {
			return false, nil
		}
	}
	return true, nil
}

// compare orders the reference's value against the comparison's. ok is false
// when the reference lacks the field; an error means the stored metadata is
// malformed for the requested comparison.
func (c comparison) compare(ref registry.Reference) (result int, ok bool, err error) {
	if c.kind == "tag" {
		// A reference without tags is unknown rather than untagged, so even
		// != does not match it, consistent with missing facts.
		if len(ref.Tags) == 0 {
			return 0, false, nil
		}
		if ref.HasTag(c.value) {
			return 0, true, nil
		}
		return 1, true, nil
	}
	if c.kind == "fact" {
		fact, ok := ref.Fact(c.field)
		if !ok {
			return 0, false, nil
		}
		switch fact.Kind {
		case registry.FactNumber:
			return cmp.Compare(fact.Number, c.number), true, nil
		case registry.FactDate:
			return fact.Date.Compare(c.date), true, nil
		default:
			return strings.Compare(fact.String, c.value), true, nil
		}
	}
	value := ref.Metadata[c.field]
	if c.kind == "identity" {
		switch c.field {
		case "name":
			value = ref.Name
		case "version":
			value = ref.Version
		case "registry":
			value = ref.Registry
		case "namespace":
			value = ref.Namespace
		}
	}
	if value == "" {
		return 0, false, nil
	}
	switch c.kind {
	case "number":
		n, err := finiteNumber(value)
		if err != nil {
			return 0, false, fmt.Errorf("%s %s: %w", ref.Name, c.field, err)
		}
		return cmp.Compare(n, c.number), true, nil
	case "date":
		d, err := parseDate(value)
		if err != nil {
			return 0, false, fmt.Errorf("%s %s: %w", ref.Name, c.field, err)
		}
		return d.Compare(c.date), true, nil
	}
	return strings.Compare(value, c.value), true, nil
}

// Select filters an already ordered inventory, then limits matching versions.
// A limit of zero is unlimited. Each invocation starts a fresh selection.
func Select(entries registry.Entries, filter Filter, limit int) registry.Entries {
	return func(ctx context.Context, emit func(registry.Reference) error) error {
		if limit < 0 {
			return fmt.Errorf("selection limit must be nonnegative")
		}
		stop := errors.New("selection complete")
		count := 0
		err := entries(ctx, func(ref registry.Reference) error {
			ok, err := filter.Match(ref)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if err := emit(ref); err != nil {
				return err
			}
			count++
			if limit > 0 && count >= limit {
				return stop
			}
			return nil
		})
		if errors.Is(err, stop) {
			return nil
		}
		return err
	}
}
