// Package i18n provides English (default) and French strings for the UI,
// the invoice documents and the e-mails.
package i18n

import (
	"fmt"
	"strings"
	"time"
)

var Langs = []string{"en", "fr"}

func Valid(l string) bool { return l == "en" || l == "fr" }

func Norm(l string) string {
	if Valid(l) {
		return l
	}
	return "en"
}

// T translates key into lang, falling back to English then to the key.
func T(lang, key string, args ...any) string {
	s, ok := dict[key][lang]
	if !ok {
		s, ok = dict[key]["en"]
		if !ok {
			s = key
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

var monthsFR = []string{"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"}

// Date formats an ISO date (YYYY-MM-DD) for humans.
func Date(lang, iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	if lang == "fr" {
		return fmt.Sprintf("%d %s %d", t.Day(), monthsFR[t.Month()-1], t.Year())
	}
	return t.Format("Jan 2, 2006")
}

func DateTime(lang string, unix int64, loc *time.Location) string {
	if unix == 0 {
		return ""
	}
	t := time.Unix(unix, 0).In(loc)
	if lang == "fr" {
		return t.Format("02/01/2006 15:04")
	}
	return t.Format("Jan 2, 2006 15:04")
}

func Plural(lang, key string, n int) string {
	if n > 1 || (lang == "en" && n == 0) {
		return T(lang, key+".many", n)
	}
	return T(lang, key+".one", n)
}

func HasKey(key string) bool { _, ok := dict[key]; return ok }

// Lines splits multi-line text (addresses) for rendering.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
