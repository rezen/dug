package chrome

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/registry"
)

const maxDetailBytes = 4 << 20

// details are the listing facts the Web Store page publishes about an item.
// The update service carries none of them.
type details struct {
	name, developer, categoryPath, categoryName string
	users, ratings                              *int64
	rating                                      *float64 // 0–5
	// record is the verbatim item array embedded in the store page, if found.
	record json.RawMessage
}

// rawRecord is the reference's raw record: the update service's XML response,
// which is not JSON, as a string, beside the store page's verbatim item array.
func rawRecord(update []byte, record json.RawMessage) json.RawMessage {
	out, err := json.Marshal(struct {
		UpdateXML string          `json:"update_xml"`
		StoreItem json.RawMessage `json:"store_item,omitempty"`
	}{string(update), record})
	if err != nil {
		return nil
	}
	return out
}

var (
	usersText    = regexp.MustCompile(`([0-9][0-9,]*)\+?\s+users?\b`)
	ratingsText  = regexp.MustCompile(`([0-9][0-9,]*(?:\.[0-9]+)?)([KM])?\s+ratings?\b`)
	categoryLink = regexp.MustCompile(`href="[^"]*category/extensions/([A-Za-z0-9_/-]+)"[^>]*>([^<]+)</a>`)
	offeredBy    = regexp.MustCompile(`Offered by</div><div>([^<]+)</div>`)
)

// parseDetails reads an item's store page. The page is a JavaScript app, but
// it ships the item record as a JSON array inside a data blob that begins
// with the item ID: [id, icon, name, rating, rating count, image,
// description, ..., [category path], ..., users, ...]. The record is read
// first because its counts are exact; the visible text, which rounds
// ("3.8K ratings"), fills whatever the record lacks. A removed item renders
// the bare store shell, which yields nothing.
func parseDetails(id string, page []byte) details {
	var d details
	if start := bytes.Index(page, []byte(`["`+id+`",`)); start >= 0 {
		var item []any
		var record json.RawMessage
		if err := json.NewDecoder(bytes.NewReader(page[start:])).Decode(&record); err == nil && json.Unmarshal(record, &item) == nil {
			d.record = bytes.Clone(record)
			d.fromRecord(item)
		}
	}
	text := string(page)
	if d.users == nil {
		if m := usersText.FindStringSubmatch(text); m != nil {
			if n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64); err == nil {
				d.users = &n
			}
		}
	}
	if d.ratings == nil {
		if m := ratingsText.FindStringSubmatch(text); m != nil {
			if n, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil {
				switch m[2] {
				case "K":
					n *= 1e3
				case "M":
					n *= 1e6
				}
				count := int64(math.Round(n))
				d.ratings = &count
			}
		}
	}
	if m := categoryLink.FindStringSubmatch(text); m != nil {
		if d.categoryPath == "" {
			d.categoryPath = m[1]
		}
		d.categoryName = strings.TrimSpace(html.UnescapeString(m[2]))
	}
	if m := offeredBy.FindStringSubmatch(text); m != nil {
		d.developer = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	return d
}

// fromRecord takes the fields of the embedded item record by position,
// keeping only values of the expected type and range.
func (d *details) fromRecord(item []any) {
	str := func(i int) string {
		if i < len(item) {
			s, _ := item[i].(string)
			return s
		}
		return ""
	}
	count := func(i int) *int64 {
		if i < len(item) {
			if n, ok := item[i].(float64); ok && n >= 0 && n == math.Trunc(n) && n < math.MaxInt64 {
				v := int64(n)
				return &v
			}
		}
		return nil
	}
	d.name = strings.TrimSpace(str(2))
	if len(item) > 3 {
		if avg, ok := item[3].(float64); ok && avg >= 0 && avg <= 5 {
			d.rating = &avg
		}
	}
	d.ratings = count(4)
	d.users = count(14)
	if len(item) > 11 {
		if category, ok := item[11].([]any); ok && len(category) > 0 {
			if path, ok := category[0].(string); ok {
				d.categoryPath = path
			}
		}
	}
}

// apply attaches the page's facts to the reference: the user count is the
// install base, the category path becomes tags, the developer the author, and
// ratings go to metadata under the same keys the other sources use.
func (d details) apply(ref *registry.Reference) {
	if d.users != nil {
		if ref.Facts == nil {
			ref.Facts = &registry.Facts{}
		}
		ref.Facts.Installs = registry.Int64(*d.users)
	}
	if d.rating != nil {
		ref.Metadata["rating_average"] = strconv.FormatFloat(*d.rating, 'f', -1, 64)
		ref.Metadata["rating"] = strconv.FormatFloat(*d.rating*20, 'f', -1, 64)
	}
	if d.ratings != nil {
		ref.Metadata["num_ratings"] = strconv.FormatInt(*d.ratings, 10)
	}
	if d.name != "" {
		ref.Metadata["title"] = d.name
	}
	if d.categoryName != "" {
		ref.Metadata["category"] = d.categoryName
	}
	if d.categoryPath != "" {
		ref.Tags = registry.NormalizeTags(strings.Split(d.categoryPath, "/"))
	}
	if d.developer != "" {
		ref.Author = &registry.Author{Name: d.developer}
	}
}

// fetchDetails downloads the item's store page in English, so the text
// patterns match. A missing page means the item was removed or never
// published while its CRX is still served; that is not a listing failure.
func (s *Source) fetchDetails(ctx context.Context, id string) (details, error) {
	base := s.DetailURL
	if base == "" {
		base = "https://chromewebstore.google.com/detail/"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := httpx.Get(ctx, s.Client, base+id+"?hl=en", s.Attempts)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return details{}, nil
		}
		return details{}, err
	}
	defer response.Body.Close()
	page, err := io.ReadAll(io.LimitReader(response.Body, maxDetailBytes+1))
	if err != nil {
		return details{}, err
	}
	if len(page) > maxDetailBytes {
		return details{}, fmt.Errorf("store page exceeds %d bytes", maxDetailBytes)
	}
	return parseDetails(id, page), nil
}
