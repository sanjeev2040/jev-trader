package screen

import (
	"fmt"
	"math"
	"sort"

	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

type rankedEntry struct {
	cand    candidate
	score   float64
	contrib map[string]float64
	signals map[string]float64
}

// pick orders the scored candidates, applies the minimum score and the
// per-sector cap, and builds the top N entries with their reasons.
func pick(cfg Config, ranked []rankedEntry, refs map[string][]watchlist.NewsRef, sec sectorResult) []watchlist.Entry {
	key := func(r rankedEntry) float64 {
		if cfg.Direction == "both" {
			return math.Abs(r.score)
		}
		return r.score
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ki, kj := key(ranked[i]), key(ranked[j]); ki != kj {
			return ki > kj
		}
		return ranked[i].cand.stock.Symbol < ranked[j].cand.stock.Symbol
	})
	perSector := map[string]int{}
	var out []watchlist.Entry
	for _, r := range ranked {
		if len(out) == cfg.TopN || key(r) < cfg.MinScore {
			break
		}
		sector := r.cand.stock.Industry
		if cfg.MaxPerSector > 0 && perSector[sector] >= cfg.MaxPerSector {
			continue
		}
		perSector[sector]++
		out = append(out, entry(cfg, len(out)+1, r, refs[r.cand.stock.Symbol], sec))
	}
	return out
}

func entry(cfg Config, rank int, r rankedEntry, refs []watchlist.NewsRef, sec sectorResult) watchlist.Entry {
	s := r.cand.stock
	e := watchlist.Entry{
		Rank: rank, Key: s.Key(), Symbol: s.Symbol, Name: s.Name, Sector: s.Industry,
		Score: round(r.score, 3), Signals: map[string]float64{}, News: refs,
	}
	for name, v := range r.signals {
		e.Signals[name] = round(v, 3)
	}
	// Reasons, the signal that moved the score most first.
	names := make([]string, 0, len(r.contrib))
	for name := range r.contrib {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if ci, cj := math.Abs(r.contrib[names[i]]), math.Abs(r.contrib[names[j]]); ci != cj {
			return ci > cj
		}
		return names[i] < names[j]
	})
	if cfg.Direction == "both" && r.score < 0 {
		e.Reasons = append(e.Reasons, "short candidate: the signals point down")
	}
	for _, name := range names {
		if line := reason(name, r, refs, sec); line != "" {
			e.Reasons = append(e.Reasons, line)
		}
	}
	st := r.cand.stats
	e.Reasons = append(e.Reasons, fmt.Sprintf("liquidity: previous-day value ₹%.1f cr, average volume %s shares, last close ₹%.2f", st.PrevValue/1e7, humanCount(st.ADV), st.PrevClose))
	return e
}

func reason(name string, r rankedEntry, refs []watchlist.NewsRef, sec sectorResult) string {
	c := r.contrib[name]
	switch name {
	case SignalNews:
		if len(refs) == 0 || c == 0 {
			return ""
		}
		top := refs[0]
		return fmt.Sprintf("news %+.2f: %s (%s; Jev: impact %.1f/%d, material %.2f, direction %+.2f)",
			c, short(top.Title, 110), top.Source, top.Impact, maxImpact, top.Material, top.Direction)
	case SignalSector:
		sector := r.cand.stock.Industry
		line := fmt.Sprintf("sector %s %+.2f: members' median previous session %+.1f%%", sector, r.signals[SignalSector], sec.prev[sector])
		if g, ok := sec.gap[sector]; ok {
			line += fmt.Sprintf(", pre-open %+.1f%%", g)
		}
		if ch, ok := sec.idx[sector]; ok {
			line += fmt.Sprintf(", index %+.1f%%", ch)
		}
		return line
	case SignalGap:
		if r.cand.gap == nil {
			return ""
		}
		return fmt.Sprintf("pre-open gap %+.1f%% from the previous close of ₹%.2f", *r.cand.gap, r.cand.stats.PrevClose)
	case SignalRange:
		return fmt.Sprintf("typical daily range %.1f%%, wider than %.0f%% of the screened stocks", r.cand.stats.RangePct, (r.signals[SignalRange]+1)/2*100)
	}
	return ""
}

func humanCount(x float64) string {
	switch {
	case x >= 1e7:
		return fmt.Sprintf("%.1f crore", x/1e7)
	case x >= 1e5:
		return fmt.Sprintf("%.1f lakh", x/1e5)
	}
	return fmt.Sprintf("%.0f", x)
}
