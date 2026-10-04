package screen

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/news"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// maxRefsPerEntry is how many news items an entry keeps.
const maxRefsPerEntry = 3

// jevPricePerToken is jev-1.13.0's list price, $42 per billion input tokens
// (https://docs.typesafe.ai/models.md, read 2026-10-03); output is free. Used
// only to report the cost of a run.
const jevPricePerToken = 42e-9

// JevPricePerToken is jevPricePerToken for other packages that report a cost.
const JevPricePerToken = jevPricePerToken

// scoreNews fetches the news, maps it to the candidates, has Jev score it and
// returns each symbol's news signal in [-1, 1], the items behind it, and the
// symbols with a catalyst (see Catalyst; their net decayed news signal must also be positive), and how many
// scored items each symbol has. read is false when there was no news to score (or none could be), in which case the
// news signal is out of this run.
func scoreNews(ctx context.Context, cfg Config, d Deps, cands []candidate, file *watchlist.File) (signal map[string]float64, refs map[string][]watchlist.NewsRef, catalyst map[string]bool, counts map[string]int, read bool) {
	addSource := func(name, kind, status string, items int, detail string) {
		file.Sources = append(file.Sources, watchlist.Source{Name: name, Kind: kind, Status: status, Items: items, Detail: detail})
	}
	if len(d.News) == 0 {
		addSource("news", "news", watchlist.StatusSkipped, 0, "no news sources configured")
		return nil, nil, nil, nil, false
	}
	end := cfg.Now
	if cfg.Phase == watchlist.PhaseHistoric {
		end = time.Date(cfg.Date.Year(), cfg.Date.Month(), cfg.Date.Day(), 9, 0, 0, 0, watchlist.IST)
	}
	from := end.Add(-cfg.NewsWindow)

	var all []news.Item
	okSources := 0
	for _, r := range news.FetchAll(ctx, d.News, from, end) {
		if r.Err != nil {
			addSource(r.Source, "news", watchlist.StatusFailed, 0, r.Err.Error())
			continue
		}
		okSources++
		addSource(r.Source, "news", watchlist.StatusOK, len(r.Items), "")
		all = append(all, r.Items...)
	}
	if okSources == 0 {
		file.Notes = append(file.Notes, "No news source could be read; the news signal is not in the scores.")
		return nil, nil, nil, nil, false
	}
	items := news.Dedupe(all)

	stocks := make([]news.Company, len(cands))
	byKey := make(map[string]candidate, len(cands))
	for i, c := range cands {
		stocks[i] = news.Company{Symbol: c.stock.Symbol, Name: c.stock.Name}
		byKey[c.stock.Symbol] = c
	}
	matcher := news.NewMatcher(stocks)
	var pairs []pair
	wraps := 0
	for _, it := range items {
		syms := matcher.Match(it)
		if len(syms) > cfg.MaxSymbolsPerItem {
			wraps++
			continue
		}
		for _, s := range syms {
			pairs = append(pairs, pair{item: it, stock: byKey[s].stock})
		}
	}
	capped := 0
	if len(pairs) > cfg.MaxNewsItems {
		capped = len(pairs) - cfg.MaxNewsItems
		pairs = pairs[:cfg.MaxNewsItems]
	}
	if wraps > 0 {
		file.Notes = append(file.Notes, fmt.Sprintf("%d news items named more than %d companies (market wraps) and were not scored.", wraps, cfg.MaxSymbolsPerItem))
	}
	if capped > 0 {
		file.Notes = append(file.Notes, fmt.Sprintf("%d older news items were not scored (cap of %d per run).", capped, cfg.MaxNewsItems))
	}
	if len(pairs) == 0 {
		addSource("jev", "jev", watchlist.StatusSkipped, 0, fmt.Sprintf("no news about the screened stocks among %d items", len(items)))
		return nil, nil, nil, nil, false
	}
	if d.Jev == nil {
		addSource("jev", "jev", watchlist.StatusSkipped, 0, "no Jev client: the news was fetched but not scored")
		file.Notes = append(file.Notes, "JEV_API_KEY was not available, so the news was not scored and is not in the scores.")
		return nil, nil, nil, nil, false
	}

	// The second pass of the day finds most items already scored.
	cache := openScoreCache(cfg.CacheDir, cfg.Date.In(watchlist.IST).Format(time.DateOnly), cfg.AnonymizeNews)
	var todo []pair
	asking := map[string]bool{} // anonymised items of two companies can be the same request
	for _, p := range pairs {
		if _, ok := cache.get(d.Model, p); ok {
			continue
		}
		if k := cache.requestKey(d.Model, p); !asking[k] {
			asking[k] = true
			todo = append(todo, p)
		}
	}
	fresh, failed, err := scoreAll(ctx, d.Jev, todo, cfg.NewsConcurrency, cfg.AnonymizeNews)
	if err != nil && len(todo) == len(pairs) {
		addSource("jev", "jev", watchlist.StatusFailed, 0, err.Error())
		file.Notes = append(file.Notes, fmt.Sprintf("Jev could not score the news (%v); the news signal is not in the scores.", err))
		return nil, nil, nil, nil, false
	}
	for _, s := range fresh {
		cache.put(d.Model, s)
	}
	if err := cache.save(); err != nil {
		d.Logger.Warn("news score cache not saved", "err", err)
	}
	if cfg.AnonymizeNews {
		file.Notes = append(file.Notes, "News was scored anonymised: the company's name and symbol and every date were hidden from the text sent to Jev (the look-ahead control).")
	}
	var results []scored
	for _, p := range pairs {
		if s, ok := cache.get(d.Model, p); ok {
			results = append(results, s)
		}
	}
	inTok, outTok := 0, 0
	for _, s := range fresh {
		inTok += s.InputTokens
		outTok += s.OutputTokens
	}
	detail := fmt.Sprintf("model %s: %d (item, company) pairs scored (%d asked now, %d from today's earlier pass), %d failed; %d input tokens, %d output tokens, about $%.4f at list price",
		d.Model, len(results), len(fresh), len(results)-len(fresh), failed, inTok, outTok, float64(inTok)*jevPricePerToken)
	addSource("jev", "jev", watchlist.StatusOK, len(results), detail)
	last := &file.Sources[len(file.Sources)-1]
	last.InputTokens, last.OutputTokens = inTok, outTok
	if failed > 0 {
		file.Notes = append(file.Notes, fmt.Sprintf("Jev failed on %d of %d news pairs; those are not in the scores.", failed, len(todo)))
	}

	type contribution struct {
		ref watchlist.NewsRef
		c   float64
	}
	sums := map[string]float64{}
	perSymbol := map[string][]contribution{}
	for _, s := range results {
		c := s.strength() * recency(end.Sub(s.item.Published), cfg.NewsHalfLife)
		sym := s.stock.Symbol
		sums[sym] += c
		perSymbol[sym] = append(perSymbol[sym], contribution{c: c, ref: watchlist.NewsRef{
			Source: s.item.Source, Title: s.item.Title, URL: s.item.URL, Published: s.item.Published.UTC(),
			Impact: round(s.Impact, 2), Material: round(s.Material, 2), Direction: round(s.Direction, 2),
		}})
	}
	signal = map[string]float64{}
	refs = map[string][]watchlist.NewsRef{}
	catalyst = map[string]bool{}
	counts = map[string]int{}
	for _, s := range results {
		counts[s.stock.Symbol]++
	}
	for _, s := range results {
		if cfg.Catalyst.holds(s) && sums[s.stock.Symbol] > 0 {
			catalyst[s.stock.Symbol] = true
		}
	}
	for sym, sum := range sums {
		signal[sym] = clamp(sum, -1, 1)
		cs := perSymbol[sym]
		sort.SliceStable(cs, func(i, j int) bool { return abs(cs[i].c) > abs(cs[j].c) })
		for _, c := range cs[:min(len(cs), maxRefsPerEntry)] {
			refs[sym] = append(refs[sym], c.ref)
		}
	}
	return signal, refs, catalyst, counts, true
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func round(x float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(x*p) / p
}

// short cuts s to n runes for a reason line.
func short(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
