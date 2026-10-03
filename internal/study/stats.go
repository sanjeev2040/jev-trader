package study

import (
	"math"
	"slices"
)

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// stdev is the sample standard deviation; 0 with fewer than two values.
func stdev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	s := 0.0
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(xs)-1))
}

// tStat is the t statistic of the mean of xs against zero; 0 when it is
// undefined (fewer than two values, or no spread).
func tStat(xs []float64) float64 {
	sd := stdev(xs)
	if len(xs) < 2 || sd == 0 {
		return 0
	}
	return mean(xs) / (sd / math.Sqrt(float64(len(xs))))
}

// pearson is the correlation of xs and ys; ok is false when it is undefined
// (fewer than three pairs, or one side has no spread).
func pearson(xs, ys []float64) (r float64, ok bool) {
	if len(xs) != len(ys) || len(xs) < 3 {
		return 0, false
	}
	mx, my := mean(xs), mean(ys)
	var sxy, sxx, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0, false
	}
	return sxy / math.Sqrt(sxx*syy), true
}

// spearman is the rank correlation of xs and ys; ties share their mean rank.
func spearman(xs, ys []float64) (rho float64, ok bool) {
	if len(xs) != len(ys) {
		return 0, false
	}
	return pearson(ranks(xs), ranks(ys))
}

func ranks(xs []float64) []float64 {
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		switch {
		case xs[a] < xs[b]:
			return -1
		case xs[a] > xs[b]:
			return 1
		}
		return 0
	})
	out := make([]float64, len(xs))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		r := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = r
		}
		i = j + 1
	}
	return out
}
