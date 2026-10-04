// Package geo provides postal address autocompletion: swisstopo for Swiss
// addresses (official building register), and OpenStreetMap (Photon) or
// Google Places for the rest of the world. Requests are made by the server so
// the browser only ever talks to Invoicer.
package geo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ProviderOSM    = "osm"
	ProviderGoogle = "google"
	ProviderOff    = "off"
)

// Config is the instance-wide autocomplete setup.
type Config struct {
	Swisstopo bool   // use swisstopo for Swiss addresses
	Provider  string // osm, google or off (addresses outside Switzerland)
	GoogleKey string
}

func (c Config) Enabled() bool {
	return c.Swisstopo || c.Provider == ProviderOSM || c.Provider == ProviderGoogle
}

// Suggestion is one proposed address.
type Suggestion struct {
	Label  string   `json:"label"`
	Lines  []string `json:"lines,omitempty"` // ready to paste; empty when ID must be resolved
	ID     string   `json:"id,omitempty"`    // Google place id (resolved with Place)
	Source string   `json:"source"`
}

var client = &http.Client{Timeout: 5 * time.Second}

// UserAgent identifies requests to public services (OSM usage policy).
var UserAgent = "Invoicer (self-hosted invoicing; https://github.com/flocom/Invoicer)"

// ---------- cache ----------

type cacheEntry struct {
	at  time.Time
	val []Suggestion
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

func cached(key string) ([]Suggestion, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e, ok := cache[key]
	if !ok || time.Since(e.at) > time.Hour {
		return nil, false
	}
	return e.val, true
}

func store(key string, v []Suggestion) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if len(cache) > 5000 {
		cache = map[string]cacheEntry{}
	}
	cache[key] = cacheEntry{at: time.Now(), val: v}
}

// ---------- search ----------

// Search queries swisstopo and the world provider in parallel. Swiss results
// come first; when swisstopo found something, Swiss results from the other
// provider are dropped to avoid duplicates.
func Search(ctx context.Context, cfg Config, q, lang, session string) ([]Suggestion, error) {
	q = strings.TrimSpace(q)
	if len([]rune(q)) < 3 || len(q) > 200 {
		return nil, nil
	}
	if lang != "fr" {
		lang = "en"
	}
	key := fmt.Sprintf("%v|%s|%s|%s", cfg.Swisstopo, cfg.Provider, lang, strings.ToLower(q))
	if v, ok := cached(key); ok {
		return v, nil
	}
	var wg sync.WaitGroup
	var swiss, world []Suggestion
	var swissErr, worldErr error
	if cfg.Swisstopo {
		wg.Add(1)
		go func() { defer wg.Done(); swiss, swissErr = searchSwisstopo(ctx, q, lang) }()
	}
	switch cfg.Provider {
	case ProviderOSM:
		wg.Add(1)
		go func() { defer wg.Done(); world, worldErr = searchPhoton(ctx, q, lang) }()
	case ProviderGoogle:
		if cfg.GoogleKey != "" {
			wg.Add(1)
			go func() { defer wg.Done(); world, worldErr = searchGoogle(ctx, cfg.GoogleKey, q, lang, session) }()
		}
	}
	wg.Wait()
	out := append([]Suggestion{}, swiss...)
	for _, s := range world {
		if len(swiss) > 0 && s.Source != ProviderGoogle && isSwiss(s) {
			continue
		}
		out = append(out, s)
	}
	// Rank by how many typed words a suggestion contains, so "rue de la Paix
	// Paris" prefers Paris over a similar Swiss street; swisstopo wins ties.
	words := tokens(q)
	sort.SliceStable(out, func(i, j int) bool { return score(out[i], words) > score(out[j], words) })
	seen := map[string]bool{}
	dedup := out[:0]
	for _, s := range out {
		k := strings.ToLower(s.Label)
		if !seen[k] {
			seen[k] = true
			dedup = append(dedup, s)
		}
	}
	if len(dedup) > 8 {
		dedup = dedup[:8]
	}
	if len(dedup) == 0 && swissErr != nil && worldErr != nil {
		return nil, errors.Join(swissErr, worldErr)
	}
	if swissErr == nil && worldErr == nil {
		store(key, dedup)
	}
	return dedup, nil
}

var fold = strings.NewReplacer("à", "a", "â", "a", "ä", "a", "á", "a", "ç", "c", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"î", "i", "ï", "i", "í", "i", "ô", "o", "ö", "o", "ó", "o", "ù", "u", "û", "u", "ü", "u", "ú", "u", "ß", "ss", "-", " ", ",", " ")

func tokens(s string) []string {
	var out []string
	for _, w := range strings.Fields(fold.Replace(strings.ToLower(s))) {
		if len([]rune(w)) >= 2 {
			out = append(out, w)
		}
	}
	return out
}

func score(s Suggestion, words []string) int {
	label := " " + strings.Join(tokens(s.Label+" "+strings.Join(s.Lines, " ")), " ") + " "
	n := 0
	for _, w := range words {
		if strings.Contains(label, " "+w) {
			n++
		}
	}
	return n
}

func isSwiss(s Suggestion) bool {
	if len(s.Lines) == 0 {
		return false
	}
	last := s.Lines[len(s.Lines)-1]
	return last == countryName("CH", "en") || last == countryName("CH", "fr") || last == "Schweiz/Suisse/Svizzera/Svizra"
}

func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", req.URL.Host, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// ---------- swisstopo ----------

var tagRe = regexp.MustCompile(`<[^>]*>`)

func searchSwisstopo(ctx context.Context, q, lang string) ([]Suggestion, error) {
	v := url.Values{}
	v.Set("searchText", q)
	v.Set("type", "locations")
	v.Set("origins", "address")
	v.Set("limit", "6")
	v.Set("lang", map[string]string{"fr": "fr", "en": "en"}[lang])
	var res struct {
		Results []struct {
			Attrs struct {
				Label string `json:"label"`
			} `json:"attrs"`
		} `json:"results"`
	}
	if err := getJSON(ctx, "https://api3.geo.admin.ch/rest/services/api/SearchServer?"+v.Encode(), &res); err != nil {
		return nil, err
	}
	var out []Suggestion
	for _, r := range res.Results {
		// "Bahnhofstrasse 10 <b>8001 Zürich</b>"
		street, place, ok := strings.Cut(r.Attrs.Label, "<b>")
		street = strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(street, "")))
		place = strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(place, "")))
		if !ok || street == "" || place == "" {
			continue
		}
		lines := []string{street, place, countryName("CH", lang)}
		out = append(out, Suggestion{Label: street + ", " + place, Lines: lines, Source: "swisstopo"})
	}
	return out, nil
}

// ---------- OpenStreetMap (Photon) ----------

func searchPhoton(ctx context.Context, q, lang string) ([]Suggestion, error) {
	v := url.Values{}
	v.Set("q", q)
	v.Set("limit", "6")
	// no "lang": keep local postal names (London, not Londres); only the
	// country line is translated
	var res struct {
		Features []struct {
			Properties struct {
				Name        string `json:"name"`
				HouseNumber string `json:"housenumber"`
				Street      string `json:"street"`
				Postcode    string `json:"postcode"`
				City        string `json:"city"`
				State       string `json:"state"`
				Country     string `json:"country"`
				CountryCode string `json:"countrycode"`
				Type        string `json:"type"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := getJSON(ctx, "https://photon.komoot.io/api/?"+v.Encode(), &res); err != nil {
		return nil, err
	}
	var out []Suggestion
	for _, f := range res.Features {
		p := f.Properties
		cc := strings.ToUpper(p.CountryCode)
		var lines []string
		switch {
		case p.Street != "":
			lines = append(lines, streetLine(cc, p.Street, p.HouseNumber))
		case p.Name != "" && p.Name != p.City:
			lines = append(lines, p.Name)
		}
		if pl := placeLine(cc, p.Postcode, p.City, p.State); pl != "" {
			lines = append(lines, pl)
		}
		if len(lines) == 0 {
			continue
		}
		country := p.Country
		if cc != "" {
			if n := countryName(cc, lang); n != "" {
				country = n
			}
		}
		if country != "" {
			lines = append(lines, country)
		}
		out = append(out, Suggestion{Label: strings.Join(lines, ", "), Lines: lines, Source: ProviderOSM})
	}
	return out, nil
}

// House number before the street name in these countries.
var numberFirst = map[string]bool{"FR": true, "LU": true, "MC": true, "GB": true, "IE": true, "US": true, "CA": true, "AU": true, "NZ": true}

func streetLine(cc, street, number string) string {
	if number == "" {
		return street
	}
	if numberFirst[cc] {
		return number + " " + street
	}
	return street + " " + number
}

func placeLine(cc, postcode, city, state string) string {
	switch cc {
	case "US", "CA", "AU":
		s := city
		if state != "" {
			s = strings.TrimPrefix(s+", "+state, ", ")
		}
		return strings.TrimSpace(s + " " + postcode)
	case "GB", "IE":
		return strings.TrimSpace(city + " " + postcode)
	}
	return strings.TrimSpace(postcode + " " + city)
}

// ---------- Google Places (API "New") ----------

func googleRequest(ctx context.Context, key, method, u string, body any, fieldMask string, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Goog-Api-Key", key)
	req.Header.Set("Content-Type", "application/json")
	if fieldMask != "" {
		req.Header.Set("X-Goog-FieldMask", fieldMask)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error.Message == "" {
			e.Error.Message = resp.Status
		}
		return fmt.Errorf("google: %s", e.Error.Message)
	}
	return json.Unmarshal(raw, out)
}

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
var placeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,300}$`)

func searchGoogle(ctx context.Context, key, q, lang, session string) ([]Suggestion, error) {
	body := map[string]any{"input": q, "languageCode": lang}
	if sessionRe.MatchString(session) {
		body["sessionToken"] = session
	}
	var res struct {
		Suggestions []struct {
			PlacePrediction struct {
				PlaceID string `json:"placeId"`
				Text    struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"placePrediction"`
		} `json:"suggestions"`
	}
	if err := googleRequest(ctx, key, http.MethodPost, "https://places.googleapis.com/v1/places:autocomplete", body, "", &res); err != nil {
		return nil, err
	}
	var out []Suggestion
	for _, s := range res.Suggestions {
		p := s.PlacePrediction
		if p.PlaceID == "" || !placeIDRe.MatchString(p.PlaceID) {
			continue
		}
		out = append(out, Suggestion{Label: p.Text.Text, ID: p.PlaceID, Source: ProviderGoogle})
	}
	return out, nil
}

// Place resolves a Google suggestion into address lines.
func Place(ctx context.Context, key, id, lang, session string) ([]string, error) {
	if key == "" || !placeIDRe.MatchString(id) {
		return nil, errors.New("invalid place")
	}
	if lang != "fr" {
		lang = "en"
	}
	v := url.Values{}
	v.Set("languageCode", lang)
	if sessionRe.MatchString(session) {
		v.Set("sessionToken", session)
	}
	var res struct {
		FormattedAddress string `json:"formattedAddress"`
	}
	if err := googleRequest(ctx, key, http.MethodGet, "https://places.googleapis.com/v1/places/"+url.PathEscape(id)+"?"+v.Encode(),
		nil, "formattedAddress", &res); err != nil {
		return nil, err
	}
	var lines []string
	for _, p := range strings.Split(res.FormattedAddress, ", ") {
		if p = strings.TrimSpace(p); p != "" {
			lines = append(lines, p)
		}
	}
	return lines, nil
}

// CheckGoogleKey validates a key with a tiny autocomplete request.
func CheckGoogleKey(ctx context.Context, key string) error {
	_, err := searchGoogle(ctx, key, "Bahnhofstrasse Zürich", "en", "")
	return err
}
