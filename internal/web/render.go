package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/brand"
	"github.com/flocom/invoicer/internal/config"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/store"
)

type templates struct {
	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"lines": i18n.Lines,
	"add":   func(a, b int) int { return a + b },
	"upper": strings.ToUpper,
	"join":  strings.Join,
	"contains": func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	},
	"currencies": func() []string { return money.Currencies },
	"langs":      func() []string { return i18n.Langs },
	"methods":    func() []string { return store.PaymentMethods },
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
	"input":    func(v int64) string { return money.Input(v, 2, true) },
	"qtyInput": func(v int64) string { return money.Input(v, 3, false) },
	"rateInput": func(v int64) string {
		return money.Input(v, 2, false)
	},
	"initial": func(s string) string {
		for _, r := range strings.TrimSpace(s) {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"sub100":   func(v int) int { return 100 - v },
	"readable": brand.Readable,
	"iban": func(s string) string {
		s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
		var b strings.Builder
		for i, r := range s {
			if i > 0 && i%4 == 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
		}
		return b.String()
	},
	"monthShort": func(lang, ym string) string {
		t, err := time.Parse("2006-01", ym)
		if err != nil {
			return ym
		}
		if lang == "fr" {
			return []string{"janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.", "déc."}[t.Month()-1]
		}
		return t.Format("Jan")
	},
	"bytes": func(n int64) string {
		switch {
		case n >= 1<<20:
			return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
		case n >= 1<<10:
			return fmt.Sprintf("%d KB", n>>10)
		}
		return fmt.Sprintf("%d B", n)
	},
	"datauri": func(mime string, b []byte) template.URL {
		return template.URL("data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b))
	},
}

func loadTemplates() (*templates, error) {
	t := &templates{pages: map[string]*template.Template{}}
	base, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return nil, err
	}
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		name := path.Base(f)
		if name == "layout.html" || name == "partials.html" {
			continue
		}
		clone, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := clone.ParseFS(assets, f); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		t.pages[strings.TrimSuffix(name, ".html")] = clone
	}
	return t, nil
}

// Page is the data passed to every template.
type Page struct {
	Title      string
	Nav        string
	Lang       string
	CSRF       string
	User       *store.User
	Company    *store.Company
	Companies  []*store.Company
	Flash      *Flash
	Error      string
	Data       any
	Version    string
	AssetVer   string
	BaseURL    string
	NewOrigin  string // detected origin differs from the stored one (admin banner)
	UpdateNote string
	Bare       bool // no navigation (login, setup, public pages)
	loc        *time.Location
	today      string
}

type Flash struct {
	Kind string `json:"k"` // ok, err, info
	Msg  string `json:"m"`
}

func (p *Page) T(key string, a ...any) string { return i18n.T(p.Lang, key, a...) }

// TS translates a key built dynamically (e.g. "status." + .Status).
func (p *Page) TS(prefix, key string) string { return i18n.T(p.Lang, prefix+key) }

func (p *Page) Money(v int64, cur string) string { return money.Format(v, cur, p.Lang) }
func (p *Page) Totals(m map[string]int64) []string {
	var out []string
	for _, c := range money.Currencies {
		if v, ok := m[c]; ok && v != 0 {
			out = append(out, money.Format(v, c, p.Lang))
		}
	}
	return out
}
func (p *Page) Date(iso string) string         { return i18n.Date(p.Lang, iso) }
func (p *Page) DateTime(unix int64) string     { return i18n.DateTime(p.Lang, unix, p.loc) }
func (p *Page) Qty(milli int64) string         { return money.Quantity(milli, p.Lang) }
func (p *Page) Rate(bp int64) string           { return money.Rate(bp, p.Lang) }
func (p *Page) Today() string                  { return p.today }
func (p *Page) Status(i *store.Invoice) string { return i.DisplayStatus(p.today) }
func (p *Page) CPath(format string, a ...any) string {
	if p.Company == nil {
		return "/"
	}
	return fmt.Sprintf("/c/%d", p.Company.ID) + fmt.Sprintf(format, a...)
}

func (s *Server) page(c *Ctx, title, nav string, data any) *Page {
	p := &Page{Title: title, Nav: nav, Lang: c.Lang, CSRF: c.csrfToken(), User: c.User, Company: c.Company, Data: data,
		Version: config.Version, AssetVer: s.assetVer, BaseURL: s.App.BaseURL(), loc: s.App.Location(), today: s.App.Today()}
	if c.User != nil {
		p.Companies, _ = s.Store.CompaniesFor(c.User, false)
		if c.User.IsOwner() {
			if o := s.detectOrigin(c.R); o != "" && o != p.BaseURL && p.BaseURL != "" {
				p.NewOrigin = o
			}
		}
		if c.User.IsOwner() && s.App.Updater != nil {
			st := s.App.Updater.Status()
			if st.Required {
				p.UpdateNote = "required"
			} else if st.Available {
				p.UpdateNote = st.Latest
			}
		}
	} else {
		p.Bare = true
	}
	p.Flash = s.popFlash(c)
	return p
}

func (s *Server) render(c *Ctx, status int, name string, p *Page) error {
	t, ok := s.tpl.pages[name]
	if !ok {
		return fmt.Errorf("unknown template %s", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", p); err != nil {
		return err
	}
	h := c.W.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	c.W.WriteHeader(status)
	_, err := c.W.Write(buf.Bytes())
	return err
}

func (s *Server) renderError(c *Ctx, status int, key string) {
	p := s.page(c, c.t("err.title"), "", map[string]any{"Status": status, "Message": c.t(key)})
	p.Bare = c.User == nil
	if err := s.render(c, status, "error", p); err != nil {
		slog.Error("render error page", "err", err)
		http.Error(c.W, http.StatusText(status), status)
	}
}

// ---------- flash messages ----------

func (c *Ctx) flash(kind, msg string) {
	b, _ := json.Marshal(Flash{Kind: kind, Msg: msg})
	http.SetCookie(c.W, &http.Cookie{Name: cookieFlash, Value: c.s.signPreCSRF(base64.RawURLEncoding.EncodeToString(b)), Path: "/",
		HttpOnly: true, Secure: c.s.isHTTPS(c.R), SameSite: http.SameSiteLaxMode, MaxAge: 60})
}

func (c *Ctx) ok(key string, a ...any)  { c.flash("ok", c.t(key, a...)) }
func (c *Ctx) bad(key string, a ...any) { c.flash("err", c.t(key, a...)) }

func (s *Server) popFlash(c *Ctx) *Flash {
	ck, err := c.R.Cookie(cookieFlash)
	if err != nil || ck.Value == "" {
		return nil
	}
	http.SetCookie(c.W, &http.Cookie{Name: cookieFlash, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.isHTTPS(c.R)})
	// signed so another site or sub-domain cannot plant fake messages
	if !s.validPreCSRF(ck.Value) {
		return nil
	}
	raw, _, _ := strings.Cut(ck.Value, ".")
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) > 2000 {
		return nil
	}
	var f Flash
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	if f.Kind != "ok" && f.Kind != "err" && f.Kind != "info" {
		f.Kind = "info"
	}
	return &f
}
