// Package money handles amounts stored as integer minor units (cents),
// quantities stored in thousandths and tax rates in basis points.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var Currencies = []string{"EUR", "USD", "CAD", "CHF", "GBP"}

func ValidCurrency(c string) bool {
	for _, x := range Currencies {
		if x == c {
			return true
		}
	}
	return false
}

var symbols = map[string]string{"EUR": "€", "USD": "$", "CAD": "$", "CHF": "CHF", "GBP": "£"}

// ParseDecimal parses user input such as "1 234,56", "1,234.56" or "12.5"
// into an integer scaled by 10^scale. It accepts both comma and dot as the
// decimal separator.
func ParseDecimal(s string, scale int) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer(" ", "", " ", "", " ", "", "'", "", "€", "", "$", "", "£", "").Replace(s)
	if s == "" {
		return 0, nil
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	lastComma, lastDot := strings.LastIndex(s, ","), strings.LastIndex(s, ".")
	sep := -1
	if lastComma > lastDot {
		sep = lastComma
	} else if lastDot >= 0 {
		sep = lastDot
	}
	intPart, frac := s, ""
	// a separator that appears several times ("1,000,000") is a thousands
	// separator, not the decimal point
	if sep >= 0 && strings.Count(s, s[sep:sep+1]) > 1 {
		sep = -1
	}
	if sep >= 0 {
		intPart, frac = s[:sep], s[sep+1:]
	}
	// thousands separators must delimit groups of exactly three digits
	if groups := strings.FieldsFunc(intPart, func(r rune) bool { return r == ',' || r == '.' }); len(groups) > 1 ||
		strings.ContainsAny(intPart, ",.") {
		if len(groups) == 0 || len(groups[0]) > 3 || strings.HasPrefix(intPart, ",") || strings.HasPrefix(intPart, ".") {
			return 0, errors.New("invalid number")
		}
		for _, g := range groups[1:] {
			if len(g) != 3 {
				return 0, errors.New("invalid number")
			}
		}
		if strings.Count(intPart, ",")+strings.Count(intPart, ".") != len(groups)-1 {
			return 0, errors.New("invalid number")
		}
	}
	intPart = strings.NewReplacer(",", "", ".", "").Replace(intPart)
	if intPart == "" {
		intPart = "0"
	}
	for _, r := range intPart + frac {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid number")
		}
	}
	if len(frac) > scale {
		// round half up on the extra digits
		extra := frac[scale:]
		frac = frac[:scale]
		v, err := combine(intPart, frac, scale)
		if err != nil {
			return 0, err
		}
		if extra[0] >= '5' {
			v++
		}
		if neg {
			v = -v
		}
		return v, nil
	}
	v, err := combine(intPart, frac, scale)
	if err != nil {
		return 0, err
	}
	if neg {
		v = -v
	}
	return v, nil
}

func combine(intPart, frac string, scale int) (int64, error) {
	for len(frac) < scale {
		frac += "0"
	}
	if len(intPart) > 13 {
		return 0, errors.New("number too large")
	}
	v, err := strconv.ParseInt(intPart+frac, 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func ParseAmount(s string) (int64, error)   { return ParseDecimal(s, 2) }
func ParseQuantity(s string) (int64, error) { return ParseDecimal(s, 3) }
func ParseRate(s string) (int64, error)     { return ParseDecimal(s, 2) } // percent -> basis points

// LineAmount computes quantity × unit price, rounded half away from zero.
func LineAmount(qtyMilli, unitCents int64) int64 {
	return roundDiv(qtyMilli*unitCents, 1000)
}

// Tax computes base × rate (basis points), rounded.
func Tax(base, bp int64) int64 { return roundDiv(base*bp, 10000) }

func roundDiv(n, d int64) int64 {
	if n >= 0 {
		return (n + d/2) / d
	}
	return -((-n + d/2) / d)
}

// group inserts a thousands separator.
func group(s, sep string) string {
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func formatScaled(v int64, scale int, minFrac int, lang string) string {
	neg := v < 0
	if neg {
		v = -v
	}
	pow := int64(math.Pow10(scale))
	ip := strconv.FormatInt(v/pow, 10)
	fp := fmt.Sprintf("%0*d", scale, v%pow)
	for len(fp) > minFrac && strings.HasSuffix(fp, "0") {
		fp = fp[:len(fp)-1]
	}
	thousands, dec := ",", "."
	if lang == "fr" {
		thousands, dec = " ", ","
	}
	out := group(ip, thousands)
	if fp != "" {
		out += dec + fp
	}
	if neg {
		out = "-" + out
	}
	return out
}

// Number formats an amount in cents without currency symbol.
func Number(cents int64, lang string) string { return formatScaled(cents, 2, 2, lang) }

// Format formats an amount with its currency following the language convention.
func Format(cents int64, currency, lang string) string {
	n := Number(cents, lang)
	sym := symbols[currency]
	switch {
	case currency == "CHF":
		return "CHF " + n
	case lang == "fr":
		if currency == "USD" {
			sym = "$ US"
		} else if currency == "CAD" {
			sym = "$ CA"
		}
		return n + " " + sym
	default:
		switch currency {
		case "CAD":
			return "CA$" + n
		}
		if strings.HasPrefix(n, "-") {
			return "-" + sym + n[1:]
		}
		return sym + n
	}
}

// Quantity formats a quantity stored in thousandths, trimming useless zeros.
func Quantity(milli int64, lang string) string { return formatScaled(milli, 3, 0, lang) }

// Rate formats basis points as a percentage ("20", "5.5").
func Rate(bp int64, lang string) string { return formatScaled(bp, 2, 0, lang) }

// Input formats a value for an <input> field (dot decimal, no grouping).
// keepCents always shows two decimals (for prices).
func Input(v int64, scale int, keepCents bool) string {
	neg := v < 0
	if neg {
		v = -v
	}
	pow := int64(math.Pow10(scale))
	s := strconv.FormatInt(v/pow, 10)
	fs := fmt.Sprintf("%0*d", scale, v%pow)
	if !keepCents {
		fs = strings.TrimRight(fs, "0")
	}
	if fs != "" {
		s += "." + fs
	}
	if neg {
		s = "-" + s
	}
	return s
}
