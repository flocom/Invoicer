package web

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/geo"
	"github.com/flocom/invoicer/internal/money"
)

func (c *Ctx) json(status int, v any) error {
	h := c.W.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	c.W.WriteHeader(status)
	return json.NewEncoder(c.W).Encode(v)
}

func attribution(cfg geo.Config) string {
	var parts []string
	if cfg.Swisstopo {
		parts = append(parts, "© swisstopo")
	}
	switch {
	case cfg.Provider == geo.ProviderOSM:
		parts = append(parts, "© OpenStreetMap contributors")
	case cfg.Provider == geo.ProviderGoogle && cfg.GoogleKey != "":
		parts = append(parts, "Powered by Google")
	}
	return strings.Join(parts, " · ")
}

// addressSearch proxies address autocompletion (authenticated users only).
func (s *Server) addressSearch(c *Ctx) error {
	cfg := s.App.AddressConfig()
	if !cfg.Enabled() {
		return c.json(200, map[string]any{"suggestions": []any{}})
	}
	if !s.limiter.allow("addr:"+itoa(c.User.ID), 60, time.Minute) {
		return c.json(429, map[string]string{"error": "rate limited"})
	}
	q := c.R.URL.Query()
	ctx, cancel := context.WithTimeout(c.R.Context(), 6*time.Second)
	defer cancel()
	lang := c.Lang
	if l := q.Get("lang"); l == "en" || l == "fr" {
		lang = l // the client's document language (country line)
	}
	res, err := geo.Search(ctx, cfg, q.Get("q"), lang, q.Get("s"))
	if err != nil {
		return c.json(502, map[string]string{"error": "lookup failed"})
	}
	if res == nil {
		res = []geo.Suggestion{}
	}
	return c.json(200, map[string]any{"suggestions": res, "attribution": attribution(cfg)})
}

// addressPlace resolves a Google suggestion into address lines.
func (s *Server) addressPlace(c *Ctx) error {
	cfg := s.App.AddressConfig()
	if cfg.Provider != geo.ProviderGoogle || cfg.GoogleKey == "" {
		return c.json(404, map[string]string{"error": "not available"})
	}
	if !s.limiter.allow("addr:"+itoa(c.User.ID), 60, time.Minute) {
		return c.json(429, map[string]string{"error": "rate limited"})
	}
	q := c.R.URL.Query()
	ctx, cancel := context.WithTimeout(c.R.Context(), 6*time.Second)
	defer cancel()
	lines, err := geo.Place(ctx, cfg.GoogleKey, q.Get("id"), c.Lang, q.Get("s"))
	if err != nil {
		return c.json(502, map[string]string{"error": "lookup failed"})
	}
	return c.json(200, map[string]any{"lines": lines})
}

// lineSuggest returns previously invoiced products and services.
func (s *Server) lineSuggest(c *Ctx) error {
	q := strings.TrimSpace(c.R.URL.Query().Get("q"))
	if len([]rune(q)) < 2 || len(q) > 200 {
		return c.json(200, map[string]any{"suggestions": []any{}})
	}
	list, err := s.Store.LineSuggestions(c.Company.ID, q, 8)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Price = money.Input(list[i].UnitPrice, 2, true)
		list[i].Tax = money.Input(list[i].TaxBP, 2, false)
	}
	if list == nil {
		return c.json(200, map[string]any{"suggestions": []any{}})
	}
	return c.json(200, map[string]any{"suggestions": list})
}

func (s *Server) systemAddress(c *Ctx) error {
	provider := c.form("provider")
	if provider != geo.ProviderOSM && provider != geo.ProviderGoogle && provider != geo.ProviderOff {
		provider = geo.ProviderOSM
	}
	key := strings.TrimSpace(c.R.PostFormValue("google_key"))
	if len(key) > 200 {
		key = ""
	}
	if key != "" {
		ctx, cancel := context.WithTimeout(c.R.Context(), 10*time.Second)
		defer cancel()
		if err := geo.CheckGoogleKey(ctx, key); err != nil {
			c.flash("err", err.Error())
			return c.redirect("/admin/system")
		}
	}
	if provider == geo.ProviderGoogle && key == "" && s.App.AddressConfig().GoogleKey == "" {
		c.bad("system.google_key_needed")
		return c.redirect("/admin/system")
	}
	if err := s.App.SaveAddressConfig(c.form("swisstopo") == "1", provider, key, c.form("remove_key") == "1"); err != nil {
		return err
	}
	c.audit("system.address", provider)
	c.ok("flash.saved")
	return c.redirect("/admin/system")
}
