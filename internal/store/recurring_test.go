package store

import "testing"

func TestAdvance(t *testing.T) {
	r := &Recurring{IntervalUnit: "month", IntervalCount: 1, AnchorDay: 31}
	seq := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30"}
	for i := 0; i < len(seq)-1; i++ {
		if got := r.Advance(seq[i]); got != seq[i+1] {
			t.Errorf("Advance(%s) = %s want %s", seq[i], got, seq[i+1])
		}
	}
	y := &Recurring{IntervalUnit: "year", IntervalCount: 1, AnchorDay: 29}
	if got := y.Advance("2028-02-29"); got != "2029-02-28" {
		t.Errorf("leap year: %s", got)
	}
	w := &Recurring{IntervalUnit: "week", IntervalCount: 2}
	if got := w.Advance("2026-12-25"); got != "2027-01-08" {
		t.Errorf("weeks: %s", got)
	}
	q := &Recurring{IntervalUnit: "month", IntervalCount: 3, AnchorDay: 15}
	if got := q.Advance("2026-11-15"); got != "2027-02-15" {
		t.Errorf("quarterly: %s", got)
	}
}
