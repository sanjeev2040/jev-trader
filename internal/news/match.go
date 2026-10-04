package news

import (
	"sort"
	"strings"
)

// Company is a listed company the Matcher can find in text.
type Company struct {
	Symbol string
	Name   string
}

// Matcher finds the companies a headline is about. A company is found by its
// name as news writes it ("KPIT Technologies", "Birlasoft"), derived from the
// registered name by dropping the legal suffix and trailing generic words, or
// by its symbol written in capitals. Names that would be ambiguous (shared by
// two companies, a single common word, or under four characters) are not
// matched, because a wrong match costs a model call and can put a stock on the
// list for news that is not about it.
type Matcher struct {
	aliases []alias
	symbols map[string]bool
}

type alias struct {
	padded string // " kpit technologies "
	symbol string
}

// legalWords end a registered name and are never part of how news writes it.
var legalWords = map[string]bool{"ltd": true, "limited": true, "pvt": true, "private": true, "co": true, "inc": true}

// genericWords may be dropped from the end of a name: "KPIT Technologies" is
// written "KPIT". Each step keeps a name that is still distinctive.
var genericWords = map[string]bool{
	"india": true, "indian": true, "industries": true, "enterprises": true, "corporation": true, "corp": true,
	"company": true, "holdings": true, "group": true, "technologies": true, "technology": true, "services": true,
	"solutions": true, "systems": true, "laboratories": true, "labs": true, "international": true, "global": true,
	"infrastructure": true, "infra": true, "engineering": true, "projects": true, "foods": true, "finance": true,
	"financial": true, "pharmaceuticals": true, "pharma": true, "healthcare": true, "energy": true, "power": true,
}

// commonNames are words and group names too common to identify one company on
// their own.
var commonNames = map[string]bool{
	"tata": true, "adani": true, "aditya": true, "birla": true, "reliance": true, "mahindra": true, "hindustan": true,
	"bharat": true, "national": true, "united": true, "general": true, "state": true, "central": true, "indian": true,
	"first": true, "great": true, "new": true, "bank": true, "gujarat": true, "bajaj": true, "godrej": true,
	"jindal": true, "kotak": true, "apollo": true, "sun": true, "oil": true, "gas": true, "steel": true,
	"cement": true, "power": true, "india": true, "global": true, "asian": true, "dalmia": true, "torrent": true,
}

// NewMatcher builds a Matcher for the companies.
func NewMatcher(companies []Company) *Matcher {
	m := &Matcher{symbols: map[string]bool{}}
	owners := map[string]map[string]bool{} // alias -> symbols
	add := func(a, symbol string) {
		if len(a) < 4 || commonNames[a] {
			return
		}
		if owners[a] == nil {
			owners[a] = map[string]bool{}
		}
		owners[a][symbol] = true
	}
	for _, c := range companies {
		m.symbols[c.Symbol] = true
		words := strings.Fields(normalize(c.Name))
		for len(words) > 1 && legalWords[words[len(words)-1]] {
			words = words[:len(words)-1]
		}
		add(strings.Join(words, " "), c.Symbol)
		for len(words) > 1 && genericWords[words[len(words)-1]] {
			words = words[:len(words)-1]
			add(strings.Join(words, " "), c.Symbol)
		}
	}
	for a, syms := range owners {
		if len(syms) != 1 {
			continue // ambiguous
		}
		for s := range syms {
			m.aliases = append(m.aliases, alias{padded: " " + a + " ", symbol: s})
		}
	}
	sort.Slice(m.aliases, func(i, j int) bool { return m.aliases[i].padded < m.aliases[j].padded })
	return m
}

// NameTerms returns the ways news may write a company's name, longest first,
// as lists of lower-case words: the registered name without its legal suffix,
// that name with trailing generic words dropped one at a time, and each
// remaining distinctive word on its own. Unlike the Matcher it keeps short and
// common names, because its caller hides a company rather than finding it:
// over-matching only removes more words.
func NameTerms(name string) [][]string {
	words := strings.Fields(normalize(name))
	for len(words) > 1 && legalWords[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	var out [][]string
	seen := map[string]bool{}
	add := func(ws []string) {
		if len(ws) == 0 {
			return
		}
		if k := strings.Join(ws, " "); !seen[k] {
			seen[k] = true
			out = append(out, ws)
		}
	}
	add(words)
	ws := words
	for len(ws) > 1 && genericWords[ws[len(ws)-1]] {
		ws = ws[:len(ws)-1]
		add(ws)
	}
	for _, w := range words {
		if len(w) >= 3 && w != "and" && w != "the" && !legalWords[w] && !genericWords[w] {
			add([]string{w})
		}
	}
	return out
}

// Match returns the symbols of the companies the item names, in symbol order.
// An item that names its own symbol (NSE announcements) is about that company
// alone.
func (m *Matcher) Match(it Item) []string {
	if it.Symbol != "" {
		if m.symbols[it.Symbol] {
			return []string{it.Symbol}
		}
		return nil
	}
	text := it.Title + " . " + it.Summary
	padded := " " + normalize(text) + " "
	found := map[string]bool{}
	for _, a := range m.aliases {
		if strings.Contains(padded, a.padded) {
			found[a.symbol] = true
		}
	}
	// A symbol written in capitals, such as "BSOFT" or "KPITTECH".
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '&' || r == '-')
	}) {
		if len(w) >= 4 && m.symbols[w] {
			found[w] = true
		}
	}
	var out []string
	for s := range found {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
