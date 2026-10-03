package screen

import (
	"sort"

	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// sectorResult holds the sector table and the signal of each sector that had
// enough members.
type sectorResult struct {
	rows  []watchlist.Sector
	score map[string]float64
	// prev and gap are each sector's median previous-session return and median
	// pre-open gap (when there is one), in percent, for the reasons.
	prev map[string]float64
	gap  map[string]float64
	idx  map[string]float64
}

// Relative weights of the parts of a sector's raw strength. The pre-open gap
// counts double because it is the freshest of them.
const (
	sectorPrevWeight = 1.0
	sectorGapWeight  = 2.0
	sectorIdxWeight  = 1.0
)

// sectorStrength groups the candidates by sector (NSE industry) and rates each
// by the median previous-session return of its liquid members, the median
// pre-open gap where there is one, and the sectoral index's move where
// quoted. The medians stop one stock's circuit move from making a sector.
func sectorStrength(cfg Config, cands []candidate, idxChange map[string]float64) sectorResult {
	type group struct{ prev, gap []float64 }
	groups := map[string]*group{}
	for _, c := range cands {
		g := groups[c.stock.Industry]
		if g == nil {
			g = &group{}
			groups[c.stock.Industry] = g
		}
		g.prev = append(g.prev, c.stats.PrevReturn)
		if c.gap != nil {
			g.gap = append(g.gap, *c.gap)
		}
	}
	res := sectorResult{score: map[string]float64{}, prev: map[string]float64{}, gap: map[string]float64{}, idx: idxChange}
	for name, g := range groups {
		row := watchlist.Sector{Name: name, Members: len(g.prev), PrevReturn: median(g.prev)}
		res.prev[name] = row.PrevReturn
		sum, weight := row.PrevReturn*sectorPrevWeight, sectorPrevWeight
		if len(g.gap) > 0 {
			m := median(g.gap)
			row.Gap = &m
			res.gap[name] = m
			sum, weight = sum+m*sectorGapWeight, weight+sectorGapWeight
		}
		if ch, ok := idxChange[name]; ok {
			c := ch
			row.IndexChange = &c
			row.Index = cfg.SectorIndexes[name]
			sum, weight = sum+ch*sectorIdxWeight, weight+sectorIdxWeight
		}
		if len(g.prev) >= cfg.MinSectorMembers {
			row.Score = squash(sum/weight, cfg.SectorScale)
			res.score[name] = row.Score
		}
		res.rows = append(res.rows, row)
	}
	sort.Slice(res.rows, func(i, j int) bool {
		if res.rows[i].Score != res.rows[j].Score {
			return res.rows[i].Score > res.rows[j].Score
		}
		return res.rows[i].Name < res.rows[j].Name
	})
	return res
}
