package domain

import (
	"testing"
	"time"
)

func TestMedianAndDeviation(t *testing.T) {
	ms := func(values ...int) []time.Duration {
		out := make([]time.Duration, len(values))
		for i, v := range values {
			out[i] = time.Duration(v) * time.Millisecond
		}
		return out
	}
	cases := []struct {
		name              string
		values            []time.Duration
		median, deviation time.Duration
	}{
		{"none", nil, 0, 0},
		{"one", ms(70), 70 * time.Millisecond, 0},
		{"two reused requests take their midpoint", ms(60, 80), 70 * time.Millisecond, 10 * time.Millisecond},
		{"unsorted odd", ms(90, 10, 50), 50 * time.Millisecond, 40 * time.Millisecond},
		{"one wild sample moves neither", ms(50, 51, 49, 50, 3000), 50 * time.Millisecond, time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Median(c.values); got != c.median {
				t.Errorf("median %v, want %v", got, c.median)
			}
			if got := MedianDeviation(c.values); got != c.deviation {
				t.Errorf("deviation %v, want %v", got, c.deviation)
			}
		})
	}
}

func TestP95(t *testing.T) {
	ds := make([]time.Duration, 20)
	for i := range ds {
		ds[i] = time.Duration(20-i) * time.Millisecond
	}
	if got := P95(ds); got != 19*time.Millisecond {
		t.Errorf("p95 of 1..20 ms = %v", got)
	}
	if got := P95([]time.Duration{7}); got != 7 {
		t.Errorf("p95 of one = %v", got)
	}
}
