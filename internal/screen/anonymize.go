package screen

import (
	"regexp"
	"strings"

	"github.com/sanjeev2040/jev-trader/internal/news"
)

// The look-ahead control. A model that was trained on the period a study
// replays can recall what happened to a named company on a named date instead
// of judging the news. The anonymised request hides what would let it: the
// company's name and symbol become "the company" and every date in the text
// becomes a placeholder, and the publication time is left out. The sector, the
// filing category and the wording of the news stay, so a signal that survives
// is one the text itself carries.

// anonCompany is what the company is called in an anonymised item.
const anonCompany = "the company"

const (
	months   = `january|february|march|april|june|july|august|september|october|november|december`
	monthAbb = `jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec`
	// monthAny matches a month written in full or abbreviated, with an optional dot.
	monthAny = `(?:` + months + `|may|` + monthAbb + `)\.?`
)

// yearRe finds a year, and the unit that follows when the number is an amount
// ("2000 crore", "2020 MW"), which anonymizeText leaves alone.
var yearRe = regexp.MustCompile(`(?i)\b(?:19|20)\d{2}\b(?:\s*(?:%|(?:crores?|cr|lakhs?|million|mn|billion|bn|mw|gw|kwh?|shares|tonnes?|units|bps)\b))?`)

// dateRes are the date shapes the text may carry, longest shape first, with
// what replaces each.
var dateRes = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\b\d{4}-\d{1,2}-\d{1,2}\b`), "[date]"},
	{regexp.MustCompile(`\b\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}\b`), "[date]"},
	{regexp.MustCompile(`(?i)\b\d{1,2}(?:st|nd|rd|th)?\s+(?:of\s+)?` + monthAny + `(?:,?\s+(?:19|20)\d{2})?`), "[date]"},
	{regexp.MustCompile(`(?i)\b` + monthAny + `\s+\d{1,2}(?:st|nd|rd|th)?(?:,?\s+(?:19|20)\d{2})?\b`), "[date]"},
	{regexp.MustCompile(`(?i)\b` + monthAny + `,?\s+(?:19|20)\d{2}\b`), "[date]"},
	{regexp.MustCompile(`(?i)\bFY\s?'?\d{2,4}(?:\s?-\s?\d{2,4})?\b`), "[period]"},
	{yearRe, "[year]"},
	{regexp.MustCompile(`(?i)\b(?:` + months + `)\b`), "[month]"},
	{regexp.MustCompile(`(?i)\b(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`), "[day]"},
	{regexp.MustCompile(`\bQ[1-4]\b`), "[quarter]"},
}

// anonymizeText hides the company (by name, by the shorter ways news writes
// it, and by symbol) and the dates in s.
func anonymizeText(s, name, symbol string) string {
	for _, c := range companyRes(name, symbol) {
		// A match consumes its boundaries, so run again for adjacent ones.
		for range 3 {
			next := c.ReplaceAllString(s, "${1}"+anonCompany+"${2}")
			if next == s {
				break
			}
			s = next
		}
	}
	for _, d := range dateRes {
		if d.re == yearRe {
			s = yearRe.ReplaceAllStringFunc(s, func(m string) string {
				if len(m) > 4 {
					return m // a year is exactly four digits: this one has a unit after it
				}
				return d.with
			})
			continue
		}
		s = d.re.ReplaceAllString(s, d.with)
	}
	return s
}

// companyRes are the patterns that find the company in text, longest first.
func companyRes(name, symbol string) []*regexp.Regexp {
	var out []*regexp.Regexp
	build := func(core string) {
		out = append(out, regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])`+core+`($|[^A-Za-z0-9])`))
	}
	for _, words := range news.NameTerms(name) {
		var b strings.Builder
		for i, w := range words {
			if i > 0 {
				if w == "and" || words[i-1] == "and" {
					b.WriteString(`\W*`)
				} else {
					b.WriteString(`\W+`)
				}
			}
			if w == "and" {
				b.WriteString(`(?:and|&)`)
				continue
			}
			b.WriteString(regexp.QuoteMeta(w))
		}
		build(b.String())
	}
	if symbol != "" {
		build(regexp.QuoteMeta(symbol))
	}
	return out
}

// anonymizedState is the news request with the company, the symbol and the
// dates hidden. The publication time is not sent at all.
func (p pair) anonymizedState() map[string]any {
	hide := func(s string) string { return anonymizeText(s, p.stock.Name, p.stock.Symbol) }
	st := map[string]any{
		"company":  anonCompany,
		"sector":   p.stock.Industry,
		"source":   p.item.Source,
		"headline": hide(p.item.Title),
	}
	if p.item.Category != "" {
		st["filing_category"] = hide(p.item.Category)
	}
	if p.item.Summary != "" {
		st["details"] = hide(p.item.Summary)
	}
	return st
}
