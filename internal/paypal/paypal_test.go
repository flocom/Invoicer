package paypal

import "testing"

func TestAmounts(t *testing.T) {
	for cents, s := range map[int64]string{0: "0.00", 5: "0.05", 12000: "120.00", 123456: "1234.56"} {
		if got := FormatAmount(cents); got != s {
			t.Errorf("FormatAmount(%d) = %s", cents, got)
		}
		if got, err := ParseAmount(s); err != nil || got != cents {
			t.Errorf("ParseAmount(%s) = %d %v", s, got, err)
		}
	}
	if got, _ := ParseAmount("12.5"); got != 1250 {
		t.Errorf("12.5 → %d", got)
	}
	if got, _ := ParseAmount("7"); got != 700 {
		t.Errorf("7 → %d", got)
	}
	for _, bad := range []string{"", "1.234", "-1", "a.b"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) should fail", bad)
		}
	}
}
