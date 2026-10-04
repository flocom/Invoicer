package money

import "testing"

func TestParseAmount(t *testing.T) {
	cases := map[string]int64{
		"12": 1200, "12.5": 1250, "12,50": 1250, "1 234,56": 123456, "1,234.56": 123456, "1.234,56": 123456,
		"1.234.567,89": 123456789, "1,000,000": 100000000, "0.005": 1, "0.004": 0, "-3.10": -310, "€ 9,99": 999, "": 0,
	}
	for in, want := range cases {
		got, err := ParseAmount(in)
		if err != nil || got != want {
			t.Errorf("ParseAmount(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "1e5", "12..5x", "1250.501250.50", "12,34.5", "1.2.3"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) should fail", bad)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		v        int64
		cur, l   string
		expected string
	}{
		{123456, "EUR", "fr", "1 234,56 €"},
		{123456, "EUR", "en", "€1,234.56"},
		{-500, "USD", "en", "-$5.00"},
		{100, "CHF", "fr", "CHF 1,00"},
		{100, "CAD", "en", "CA$1.00"},
		{100, "GBP", "en", "£1.00"},
	}
	for _, c := range cases {
		if got := Format(c.v, c.cur, c.l); got != c.expected {
			t.Errorf("Format(%d,%s,%s) = %q; want %q", c.v, c.cur, c.l, got, c.expected)
		}
	}
}

func TestLineAndTax(t *testing.T) {
	if LineAmount(2500, 40000) != 100000 {
		t.Error("2.5 × 400")
	}
	if LineAmount(333, 100) != 33 {
		t.Error("rounding")
	}
	if Tax(1999, 550) != 110 {
		t.Error("5.5% of 19.99")
	}
}
