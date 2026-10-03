package screen

import (
	"math"
	"sort"
)

// squash maps a move in percent to a signal in (-1, 1); a move of scale
// percent maps to about 0.76.
func squash(pct, scale float64) float64 { return math.Tanh(pct / scale) }

// percentile returns the signal in [-1, 1] for each value: -1 for the lowest
// and +1 for the highest, by rank. Ties share their mean rank.
func percentileSignals(values map[string]float64) map[string]float64 {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if values[keys[i]] != values[keys[j]] {
			return values[keys[i]] < values[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make(map[string]float64, len(values))
	n := len(keys)
	if n < 2 {
		for _, k := range keys {
			out[k] = 0
		}
		return out
	}
	for i := 0; i < n; {
		j := i
		for j+1 < n && values[keys[j+1]] == values[keys[i]] {
			j++
		}
		rank := float64(i+j) / 2 // mean rank of the tie group, 0..n-1
		for k := i; k <= j; k++ {
			out[keys[k]] = 2*rank/float64(n-1) - 1
		}
		i = j + 1
	}
	return out
}

// combine returns the weighted score of a stock's signals, and the weighted
// contribution of each. A signal missing for the stock counts as 0; weights of
// signals unavailable for the whole run are left out of avail so that they do
// not dilute the rest. The scale is the sum of absolute weights, so a negative
// weight (a penalty) lowers the score without shrinking the others. A signal
// with weight zero contributes nothing and is not reported.
func combine(w map[string]float64, avail map[string]bool, signals map[string]float64) (score float64, contrib map[string]float64) {
	total := 0.0
	for name, weight := range w {
		if avail[name] {
			total += math.Abs(weight)
		}
	}
	contrib = map[string]float64{}
	if total <= 0 {
		return 0, contrib
	}
	for name, weight := range w {
		if !avail[name] || weight == 0 {
			continue
		}
		c := weight / total * signals[name]
		contrib[name] = c
		score += c
	}
	return score, contrib
}
