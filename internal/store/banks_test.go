package store

import "testing"

func TestValidIBAN(t *testing.T) {
	for _, ok := range []string{"FR76 3000 6000 0112 3456 7890 189", "CH9300762011623852957", "GB82WEST12345698765432", "DE89370400440532013000"} {
		if !ValidIBAN(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"FR7630006000011234567890188", "XX00", "1234567890123456", "CH93-0076-2011"} {
		if ValidIBAN(bad) {
			t.Errorf("%s should be invalid", bad)
		}
	}
}
