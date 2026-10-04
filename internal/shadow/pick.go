package shadow

import (
	"encoding/json"

	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// PickDetail is what the log keeps of a picker entry beyond its instrument.
type PickDetail struct {
	Rank    int                `json:"rank"`
	Score   float64            `json:"score"`
	Sector  string             `json:"sector,omitempty"`
	Signals map[string]float64 `json:"signals,omitempty"`
	// News is the most material news item's Jev scores, when the entry has
	// any: impact (0..4), probability it is material and net probability the
	// price rises.
	News *NewsScores `json:"news,omitempty"`
}

// NewsScores are Jev's scores of one news item.
type NewsScores struct {
	Title     string  `json:"title"`
	Impact    float64 `json:"impact"`
	Material  float64 `json:"material"`
	Direction float64 `json:"direction"`
}

// EncodePick is the JSON the log keeps for a watchlist entry.
func EncodePick(e watchlist.Entry) json.RawMessage {
	d := PickDetail{Rank: e.Rank, Score: e.Score, Sector: e.Sector, Signals: e.Signals}
	best := -1.0
	for _, n := range e.News {
		if w := n.Impact * n.Material; w > best {
			best = w
			d.News = &NewsScores{Title: n.Title, Impact: n.Impact, Material: n.Material, Direction: n.Direction}
		}
	}
	b, err := json.Marshal(d)
	if err != nil { // a NaN score cannot be encoded; the entry is still logged, without its detail
		return nil
	}
	return b
}
