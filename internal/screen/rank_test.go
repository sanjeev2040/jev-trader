package screen

import (
	"fmt"
	"testing"

	"github.com/sanjeev2040/jev-trader/internal/universe"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

func TestDefaultListHoldsFiftyMidAndSmallCaps(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.TopN != 50 {
		t.Fatalf("default top N = %d, want 50", cfg.TopN)
	}
	if cfg.MaxPerSector < 6 { // NSE has about twenty macro industries: a lower cap could not fill 50
		t.Fatalf("max per sector %d is too low for a list of %d", cfg.MaxPerSector, cfg.TopN)
	}
}

// Seventy candidates in twelve sectors: the default config keeps the fifty
// likeliest to rise, at most MaxPerSector from one sector, best score first.
func TestPickKeepsTheFiftyLikeliestToRise(t *testing.T) {
	cfg := DefaultConfig()
	var ranked []rankedEntry
	for i := range 70 {
		ranked = append(ranked, rankedEntry{
			cand: candidate{stock: universe.Stock{
				Symbol: fmt.Sprintf("S%02d", i), Industry: fmt.Sprintf("Sector %d", i%12), Index: "NIFTY MIDCAP 150",
			}},
			score:   0.9 - float64(i)*0.01, // S00 is the best
			contrib: map[string]float64{}, signals: map[string]float64{},
		})
	}
	got := pick(cfg, ranked, map[string][]watchlist.NewsRef{}, sectorResult{})
	if len(got) != 50 {
		t.Fatalf("picked %d, want 50", len(got))
	}
	perSector := map[string]int{}
	for i, e := range got {
		if e.Rank != i+1 {
			t.Fatalf("entry %d has rank %d", i, e.Rank)
		}
		if i > 0 && e.Score > got[i-1].Score {
			t.Fatalf("entry %d (%s) outscores the one before it: not ranked by likelihood to rise", i, e.Symbol)
		}
		if perSector[e.Sector]++; perSector[e.Sector] > cfg.MaxPerSector {
			t.Fatalf("%d entries from %s, cap %d", perSector[e.Sector], e.Sector, cfg.MaxPerSector)
		}
	}
	if got[0].Symbol != "S00" {
		t.Errorf("first entry %s, want the best, S00", got[0].Symbol)
	}
}
