package screen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/news"
	"github.com/sanjeev2040/jev-trader/internal/universe"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// Deps are the screen's external systems.
type Deps struct {
	// NSE fetches the index constituent files and the ASM/GSM reports.
	NSE    universe.Fetcher
	Market Market
	News   []news.Source
	// Jev scores the news. Nil leaves the news signal out.
	Jev Asker
	// Model is the pinned Jev model id, recorded in the file.
	Model  string
	Logger *slog.Logger
}

// candidate is a universe member that passed the liquidity filters.
type candidate struct {
	stock universe.Stock
	stats stats
	gap   *float64 // percent; nil without a pre-open quote
}

// DailyLookbackDays is how many calendar days of daily candles the screen
// reads before the screened day.
const DailyLookbackDays = 45

// minGapShare is the share of candidates whose quote must differ from the
// previous close for the pre-open gap to count as read. Before pre-open prices
// exist a quote repeats the last close, and a column of zeros is not a signal.
const minGapShare = 0.10

// Run screens the universe and returns the watchlist file for cfg.Date. It
// fails only when it cannot produce a list at all (no universe, no Kite candles,
// no candidates, or a required catalyst with no news scored); a quiet day on
// which no stock has a catalyst returns an empty list, not an error. A source that fails otherwise costs its signal and is
// recorded in the file's Sources and Notes.
func Run(ctx context.Context, cfg Config, d Deps) (watchlist.File, error) {
	if err := cfg.Validate(); err != nil {
		return watchlist.File{}, err
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	date := cfg.Date.In(watchlist.IST).Format(time.DateOnly)
	file := watchlist.File{
		Version: watchlist.Version, Date: date, GeneratedAt: cfg.Now.UTC(), Phase: cfg.Phase,
		Final: cfg.Phase != watchlist.PhaseEarly, Model: d.Model, Weights: cfg.Weights.asMap(), Direction: cfg.Direction,
	}
	addSource := func(name, kind, status string, items int, detail string) {
		file.Sources = append(file.Sources, watchlist.Source{Name: name, Kind: kind, Status: status, Items: items, Detail: detail})
	}
	note := func(format string, a ...any) { file.Notes = append(file.Notes, fmt.Sprintf(format, a...)) }

	// 1. Universe.
	uni, err := universe.Load(ctx, cfg.CacheDir, cfg.Now, d.NSE)
	if err != nil {
		return file, fmt.Errorf("universe: %w", err)
	}
	addSource("nse-index-constituents", "universe", watchlist.StatusOK, len(uni.Stocks), "Nifty Midcap 150 + Smallcap 250, less Nifty 50 and Next 50, EQ series")
	if cfg.Phase == watchlist.PhaseHistoric {
		note("Index constituents are today's, not those of %s.", date)
	}
	surv, err := universe.FetchSurveillance(ctx, cfg.CacheDir, cfg.Now, d.NSE)
	switch {
	case err != nil:
		addSource("nse-asm-gsm", "surveillance", watchlist.StatusFailed, surv.Size(), err.Error())
		note("The ASM/GSM surveillance lists could not be fully read (%v); stocks under surveillance may be on the list.", err)
	default:
		addSource("nse-asm-gsm", "surveillance", watchlist.StatusOK, surv.Size(), "")
	}
	before := len(uni.Stocks)
	uni.Exclude(surv.Reasons)
	if n := before - len(uni.Stocks); n > 0 {
		note("%d universe stocks are under ASM/GSM and were left out.", n)
	}

	// 2. Daily candles: liquidity filters, previous-session returns, range.
	candles, err := fetchCandles(ctx, cfg, d, uni.Stocks, date)
	if err != nil {
		addSource("kite-historical-day", "market", watchlist.StatusFailed, 0, err.Error())
		return file, err
	}
	addSource("kite-historical-day", "market", watchlist.StatusOK, len(candles), "daily candles of the universe")
	cands, dropped := filterLiquid(cfg, uni.Stocks, candles, date)
	if len(dropped) > 0 {
		note("Liquidity filters dropped %d of %d stocks: %s.", sumCounts(dropped), len(uni.Stocks), formatCounts(dropped))
	}
	if len(cands) == 0 {
		return file, errors.New("no universe stock passes the liquidity filters")
	}

	// 3. Pre-open quotes: the gap, and the sectoral indices.
	gapRead := false
	idxChange := map[string]float64{}
	switch cfg.Phase {
	case watchlist.PhaseFinal:
		gapRead, idxChange = readQuotes(ctx, cfg, d, cands, &file)
		if !gapRead {
			for i := range cands {
				cands[i].gap = nil
			}
		}
	default:
		addSource("kite-quotes", "market", watchlist.StatusSkipped, 0, "no pre-open to read in the "+cfg.Phase+" phase")
	}

	// 4. Sectors.
	sectors := sectorStrength(cfg, cands, idxChange)
	file.Sectors = sectors.rows

	// 5. News.
	newsSignal, newsRefs, catalyst, newsRead := scoreNews(ctx, cfg, d, cands, &file)

	// 6. Combine.
	avail := map[string]bool{SignalRange: true, SignalNews: newsRead, SignalGap: gapRead, SignalSector: len(sectors.score) > 0}
	rangeSig := percentileSignals(rangeValues(cands))
	ranked := make([]rankedEntry, 0, len(cands))
	for _, c := range cands {
		sym := c.stock.Symbol
		signals := map[string]float64{SignalRange: rangeSig[sym]}
		if s, ok := sectors.score[c.stock.Industry]; ok {
			signals[SignalSector] = s
		}
		if c.gap != nil && gapRead {
			signals[SignalGap] = squash(*c.gap, cfg.GapScale)
		}
		if newsRead {
			signals[SignalNews] = newsSignal[sym]
		}
		score, contrib := combine(cfg.Weights.asMap(), avail, signals)
		ranked = append(ranked, rankedEntry{cand: c, score: score, contrib: contrib, signals: signals, catalyst: catalyst[sym]})
	}
	if cfg.Catalyst.Require {
		n := 0
		for _, r := range ranked {
			if r.catalyst {
				n++
			}
		}
		if newsRead {
			note("A material positive news catalyst is required: %d of %d screened stocks have one.", n, len(ranked))
		} else {
			note("A material positive news catalyst is required but no news was scored, so no stock can be listed.")
		}
	}
	file.Entries = pick(cfg, ranked, newsRefs, sectors)
	if cfg.KeepPool {
		for _, r := range ranked {
			p := watchlist.PoolEntry{Key: r.cand.stock.Key(), Sector: r.cand.stock.Industry, Score: round(r.score, 3), Signals: map[string]float64{}}
			for name, v := range r.signals {
				p.Signals[name] = round(v, 3)
			}
			file.Pool = append(file.Pool, p)
		}
	}
	if len(file.Entries) == 0 {
		switch {
		case !cfg.Catalyst.Require:
			return file, fmt.Errorf("no stock scored above %.2f", cfg.MinScore)
		case !newsRead:
			return file, errors.New("a news catalyst is required but no news could be scored")
		}
		file.Entries = []watchlist.Entry{}
		note("No catalysts today: no stock has a material positive news item scoring above %.2f, so the list is empty.", cfg.MinScore)
	}
	for _, name := range []string{SignalNews, SignalGap, SignalSector} {
		if !avail[name] {
			note("The %s signal was not available; its weight was left out of every score.", name)
		}
	}
	return file, nil
}

// fetchCandles returns the daily candles of the stocks, from the day's cache
// where it has them. Stocks whose request fails are left out; if most fail, or
// the failure makes the rest pointless, it fails.
func fetchCandles(ctx context.Context, cfg Config, d Deps, stocks []universe.Stock, date string) (map[string][]Candle, error) {
	cache, err := openCandleCache(cfg.CacheDir, date)
	if err != nil {
		return nil, err
	}
	day, _ := time.ParseInLocation(time.DateOnly, date, watchlist.IST)
	from, to := day.AddDate(0, 0, -DailyLookbackDays), day.Add(-time.Second)
	out := make(map[string][]Candle, len(stocks))
	var failed int
	var firstErr error
	for i, s := range stocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := s.Key()
		if c, ok := cache.get(key); ok {
			out[key] = c
			continue
		}
		c, err := d.Market.Daily(ctx, key, from, to)
		if err != nil {
			if errors.Is(err, ErrAbort) || ctx.Err() != nil {
				return nil, errors.Join(err, cache.save())
			}
			failed++
			if firstErr == nil {
				firstErr = err
			}
			d.Logger.Warn("daily candles failed", "key", key, "err", err)
			continue
		}
		cache.put(key, c)
		out[key] = c
		if (i+1)%100 == 0 {
			d.Logger.Info("daily candles", "done", i+1, "of", len(stocks))
		}
	}
	if err := cache.save(); err != nil {
		d.Logger.Warn("candle cache not saved", "err", err)
	}
	if failed*2 > len(stocks) {
		return nil, fmt.Errorf("daily candles failed for %d of %d stocks, first: %w", failed, len(stocks), firstErr)
	}
	return out, nil
}

// filterLiquid computes each stock's stats and applies the filters. It returns
// the survivors and the count of drops by reason kind.
func filterLiquid(cfg Config, stocks []universe.Stock, candles map[string][]Candle, date string) ([]candidate, map[string]int) {
	var out []candidate
	dropped := map[string]int{}
	for _, s := range stocks {
		cs, ok := candles[s.Key()]
		if !ok {
			dropped["no candles"]++
			continue
		}
		st, why := computeStats(cs, date, cfg.Filters.MinSessions)
		if why == "" {
			why = cfg.Filters.liquidity(st)
		}
		if why != "" {
			dropped[reasonKind(why)]++
			continue
		}
		out = append(out, candidate{stock: s, stats: st})
	}
	return out, dropped
}

// reasonKind reduces a drop reason to a countable kind.
func reasonKind(why string) string {
	switch {
	case strings.HasPrefix(why, "price") && strings.Contains(why, "below"):
		return "price below minimum"
	case strings.HasPrefix(why, "price"):
		return "price above maximum"
	case strings.HasPrefix(why, "previous-day value"):
		return "previous-day value too low"
	case strings.HasPrefix(why, "average volume"):
		return "average volume too low"
	case strings.HasPrefix(why, "fewer"):
		return "too little history"
	}
	return why
}

func sumCounts(m map[string]int) (n int) {
	for _, v := range m {
		n += v
	}
	return n
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func rangeValues(cands []candidate) map[string]float64 {
	m := make(map[string]float64, len(cands))
	for _, c := range cands {
		m[c.stock.Symbol] = c.stats.RangePct
	}
	return m
}

// readQuotes fetches quotes for the candidates and the sectoral indices, sets
// each candidate's gap, and reports whether the gap counts as read.
func readQuotes(ctx context.Context, cfg Config, d Deps, cands []candidate, file *watchlist.File) (gapRead bool, idxChange map[string]float64) {
	idxChange = map[string]float64{}
	keys := make([]string, 0, len(cands)+len(cfg.SectorIndexes))
	for _, c := range cands {
		keys = append(keys, c.stock.Key())
	}
	indexKeys := map[string]string{} // sector -> key, for sectors that have candidates
	seen := map[string]bool{}
	for _, c := range cands {
		if k, ok := cfg.SectorIndexes[c.stock.Industry]; ok && !seen[c.stock.Industry] {
			seen[c.stock.Industry] = true
			indexKeys[c.stock.Industry] = k
			keys = append(keys, k)
		}
	}
	quotes, err := d.Market.Quotes(ctx, keys)
	if err != nil {
		file.Sources = append(file.Sources, watchlist.Source{Name: "kite-quotes", Kind: "market", Status: watchlist.StatusFailed, Detail: err.Error()})
		file.Notes = append(file.Notes, fmt.Sprintf("Kite quotes failed (%v); the pre-open gap and sector indices are not in the scores.", err))
		return false, idxChange
	}
	moved := 0
	for i := range cands {
		q, ok := quotes[cands[i].stock.Key()]
		if !ok || q.Last <= 0 || q.PrevClose <= 0 {
			continue
		}
		g := (q.Last/q.PrevClose - 1) * 100
		cands[i].gap = &g
		if math.Abs(g) > 1e-9 {
			moved++
		}
	}
	for sector, key := range indexKeys {
		if q, ok := quotes[key]; ok && q.Last > 0 && q.PrevClose > 0 {
			idxChange[sector] = (q.Last/q.PrevClose - 1) * 100
		}
	}
	detail := fmt.Sprintf("%d of %d stocks and %d of %d sector indices quoted", countGaps(cands), len(cands), len(idxChange), len(indexKeys))
	if float64(moved) < minGapShare*float64(len(cands)) {
		file.Sources = append(file.Sources, watchlist.Source{Name: "kite-quotes", Kind: "market", Status: watchlist.StatusOK, Items: len(quotes), Detail: detail + "; prices still equal the previous close"})
		file.Notes = append(file.Notes, fmt.Sprintf("Only %d of %d stocks showed a price different from the previous close, so there was no pre-open gap to read.", moved, len(cands)))
		return false, map[string]float64{}
	}
	file.Sources = append(file.Sources, watchlist.Source{Name: "kite-quotes", Kind: "market", Status: watchlist.StatusOK, Items: len(quotes), Detail: detail})
	return true, idxChange
}

func countGaps(cands []candidate) (n int) {
	for _, c := range cands {
		if c.gap != nil {
			n++
		}
	}
	return n
}
