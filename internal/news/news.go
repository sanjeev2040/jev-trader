// Package news collects company news from free public sources for the
// pre-market screen: NSE's corporate announcements and the RSS feeds of Indian
// business news sites. It fetches and maps headlines to NSE symbols; scoring is
// the caller's job (internal/screen asks Jev).
package news

import (
	"context"
	"sort"
	"strings"
	"time"
)

// IST is the exchange's time zone.
var IST = time.FixedZone("IST", 5*3600+30*60)

// Item is one news item.
type Item struct {
	// Source is the name of the source that supplied it.
	Source string
	// ID is stable within a source, for removing duplicates.
	ID       string
	Title    string
	Summary  string
	Category string
	URL      string
	// Published is when the source says it appeared.
	Published time.Time
	// Symbol is set by a source that names the company itself (NSE
	// announcements); otherwise the Matcher finds the symbols in the text.
	Symbol string
}

// Source supplies news published in [from, to).
type Source interface {
	Name() string
	Fetch(ctx context.Context, from, to time.Time) ([]Item, error)
}

// Result is what one source returned.
type Result struct {
	Source string
	Items  []Item
	Err    error
}

// FetchAll fetches every source, one after another so that no source sees
// concurrent requests from us, and reports each source's outcome. One source
// failing does not stop the others.
func FetchAll(ctx context.Context, sources []Source, from, to time.Time) []Result {
	out := make([]Result, 0, len(sources))
	for _, s := range sources {
		items, err := s.Fetch(ctx, from, to)
		out = append(out, Result{Source: s.Name(), Items: items, Err: err})
	}
	return out
}

// Dedupe removes items seen twice, by source and id, and by identical
// normalised title (the same wire story on several feeds). It keeps the first
// and returns items newest first.
func Dedupe(items []Item) []Item {
	seen := map[string]bool{}
	var out []Item
	for _, it := range items {
		keys := []string{"id:" + it.Source + "/" + it.ID}
		if t := normalize(it.Title); t != "" && it.Symbol == "" {
			keys = append(keys, "title:"+t)
		}
		dup := false
		for _, k := range keys {
			if seen[k] {
				dup = true
			}
		}
		for _, k := range keys {
			seen[k] = true
		}
		if !dup {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Published.After(out[j].Published) })
	return out
}

// normalize lower-cases s, replaces anything that is not a letter or digit
// with a space and collapses runs of spaces, so that matching ignores
// punctuation and case. "&" reads as "and".
func normalize(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(strings.ReplaceAll(s, "&", " and ")) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			space = false
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
