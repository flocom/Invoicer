// Package web is the HTTP interface: server-rendered pages, no inline
// scripts, strict security headers, cookie sessions and CSRF protection.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/app"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

//go:embed templates static
var assets embed.FS

type Server struct {
	App        *app.App
	Store      *store.Store
	tpl        *templates
	limiter    *limiter
	trustAll   bool // TRUST_PROXY=true
	noTrust    bool // TRUST_PROXY=false (default with TLS=auto: no proxy in front)
	cloudflare bool // TRUST_PROXY=cloudflare: also honour CF-Connecting-IP
	static     http.Handler
	assetVer   string
	csrfKey    []byte
}

func New(a *app.App) (*Server, error) {
	t, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	sub, _ := fs.Sub(assets, "static")
	tp := strings.ToLower(os.Getenv("TRUST_PROXY"))
	if tp == "" && a.Cfg.TLS == "auto" {
		tp = "false" // we terminate TLS ourselves: forwarded headers can only be forged
	}
	s := &Server{App: a, Store: a.Store, tpl: t, limiter: newLimiter(), trustAll: tp == "true", noTrust: tp == "false",
		cloudflare: tp == "cloudflare",
		static:     http.FileServer(http.FS(sub)), assetVer: assetHash(sub),
		csrfKey: []byte(security.Token(32))}
	return s, nil
}

// assetHash fingerprints the static files for cache busting.
func assetHash(fsys fs.FS) string {
	h := sha256.New()
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(fsys, p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Handler builds the router wrapped with the global middleware.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()

	// public
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	m.Handle("GET /static/", http.StripPrefix("/static/", s.cacheStatic(noListing(s.static))))
	m.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("User-agent: *\nDisallow: /\n")) })
	m.HandleFunc("GET /setup", s.h(s.setupForm))
	m.HandleFunc("POST /setup", s.h(s.setupSubmit))
	m.HandleFunc("GET /login", s.h(s.loginForm))
	m.HandleFunc("POST /login", s.h(s.loginSubmit))
	m.HandleFunc("GET /login/2fa", s.h(s.mfaForm))
	m.HandleFunc("POST /login/2fa", s.h(s.mfaSubmit))
	m.HandleFunc("POST /logout", s.h(s.logout))
	m.HandleFunc("GET /invite/{token}", s.h(s.inviteForm))
	m.HandleFunc("POST /invite/{token}", s.h(s.inviteSubmit))
	m.HandleFunc("GET /reset/{token}", s.h(s.resetForm))
	m.HandleFunc("POST /reset/{token}", s.h(s.resetSubmit))
	m.HandleFunc("GET /i/{token}", s.h(s.publicInvoice))
	m.HandleFunc("GET /i/{token}/pdf", s.h(s.publicPDF))
	m.HandleFunc("GET /pay/{token}", s.h(s.publicPay))
	m.HandleFunc("GET /card/{token}", s.h(s.publicCard))
	m.HandleFunc("GET /card/{token}/setup", s.h(s.publicCardSetup))
	m.HandleFunc("POST /webhooks/stripe/{pid}", s.stripeWebhook)
	m.HandleFunc("GET /logo/{pid}", s.h(s.companyLogo))

	// authenticated
	m.HandleFunc("GET /{$}", s.h(s.auth(s.home)))
	m.HandleFunc("GET /account", s.h(s.auth(s.accountPage)))
	m.HandleFunc("POST /account/profile", s.h(s.auth(s.accountProfile)))
	m.HandleFunc("POST /account/password", s.h(s.auth(s.accountPassword)))
	m.HandleFunc("POST /account/2fa/start", s.h(s.auth(s.mfaStart)))
	m.HandleFunc("POST /account/2fa/enable", s.h(s.auth(s.mfaEnable)))
	m.HandleFunc("POST /account/2fa/disable", s.h(s.auth(s.mfaDisable)))
	m.HandleFunc("POST /account/sessions/revoke", s.h(s.auth(s.revokeSessions)))

	m.HandleFunc("GET /api/address", s.h(s.auth(s.addressSearch)))
	m.HandleFunc("GET /api/address/place", s.h(s.auth(s.addressPlace)))
	m.HandleFunc("POST /admin/system/address", s.h(s.owner(s.systemAddress)))
	m.HandleFunc("GET /companies", s.h(s.auth(s.companyList)))
	m.HandleFunc("GET /companies/new", s.h(s.admin(s.companyNew)))
	m.HandleFunc("POST /companies/new", s.h(s.admin(s.companyCreate)))

	m.HandleFunc("GET /admin/users", s.h(s.admin(s.usersPage)))
	m.HandleFunc("POST /admin/invites", s.h(s.admin(s.inviteCreate)))
	m.HandleFunc("POST /admin/invites/{id}/revoke", s.h(s.admin(s.inviteRevoke)))
	m.HandleFunc("POST /admin/users/{uid}", s.h(s.admin(s.userUpdate)))
	m.HandleFunc("POST /admin/users/{uid}/reset", s.h(s.admin(s.userReset)))
	m.HandleFunc("POST /admin/users/{uid}/delete", s.h(s.admin(s.userDelete)))
	m.HandleFunc("POST /admin/users/{uid}/transfer", s.h(s.owner(s.ownershipTransfer)))
	m.HandleFunc("GET /admin/system", s.h(s.owner(s.systemPage)))
	m.HandleFunc("POST /admin/system", s.h(s.owner(s.systemSave)))
	m.HandleFunc("POST /admin/system/domain", s.h(s.owner(s.systemDomain)))
	m.HandleFunc("POST /admin/system/update-check", s.h(s.owner(s.updateCheck)))
	m.HandleFunc("POST /admin/system/update-install", s.h(s.owner(s.updateInstall)))
	m.HandleFunc("POST /admin/system/backup", s.h(s.owner(s.backupNow)))
	m.HandleFunc("GET /admin/audit", s.h(s.admin(s.auditPage)))

	// company scoped
	c := func(p string, h handler) { m.HandleFunc(p, s.h(s.company(h))) }
	ca := func(p string, h handler) { m.HandleFunc(p, s.h(s.company(s.requireAdmin(h)))) }
	c("GET /c/{cid}", s.dashboard)
	c("GET /c/{cid}/clients", s.clientList)
	c("GET /c/{cid}/clients/new", s.clientForm)
	c("POST /c/{cid}/clients/new", s.clientSave)
	c("GET /c/{cid}/clients/{id}", s.clientView)
	c("GET /c/{cid}/clients/{id}/edit", s.clientForm)
	c("POST /c/{cid}/clients/{id}/edit", s.clientSave)
	c("POST /c/{cid}/clients/{id}/delete", s.clientDelete)
	c("POST /c/{cid}/clients/{id}/copy", s.clientCopy)
	c("POST /c/{cid}/clients/{id}/charge", s.clientCharge)
	c("POST /c/{cid}/clients/{id}/card-link", s.clientCardLink)
	c("POST /c/{cid}/clients/{id}/cards/{card}/default", s.clientCardDefault)
	c("POST /c/{cid}/clients/{id}/cards/{card}/delete", s.clientCardDelete)
	c("GET /c/{cid}/api/lines", s.lineSuggest)
	c("POST /c/{cid}/api/lines/hide", s.lineSuggestHide)
	c("POST /c/{cid}/api/clients", s.clientQuickCreate)

	c("GET /c/{cid}/invoices", s.invoiceList)
	c("GET /c/{cid}/emails", s.emailList)
	c("GET /c/{cid}/emails/{id}", s.emailView)
	c("GET /c/{cid}/emails/{id}/html", s.emailHTML)
	c("GET /c/{cid}/invoices/new", s.invoiceForm)
	c("POST /c/{cid}/invoices/new", s.invoiceSave)
	c("GET /c/{cid}/invoices/{id}", s.invoiceView)
	c("GET /c/{cid}/invoices/{id}/edit", s.invoiceForm)
	c("POST /c/{cid}/invoices/{id}/edit", s.invoiceSave)
	c("GET /c/{cid}/invoices/{id}/pdf", s.invoicePDF)
	c("POST /c/{cid}/invoices/{id}/issue", s.invoiceIssue)
	c("POST /c/{cid}/invoices/{id}/send", s.invoiceSend)
	c("POST /c/{cid}/invoices/{id}/remind", s.invoiceRemind)
	c("POST /c/{cid}/invoices/{id}/mark-sent", s.invoiceMarkSent)
	c("POST /c/{cid}/invoices/{id}/payments", s.paymentAdd)
	c("POST /c/{cid}/invoices/{id}/payments/{pid}/delete", s.paymentDelete)
	c("POST /c/{cid}/invoices/{id}/void", s.invoiceVoid)
	c("POST /c/{cid}/invoices/{id}/delete", s.invoiceDelete)
	c("POST /c/{cid}/invoices/{id}/duplicate", s.invoiceDuplicate)
	c("POST /c/{cid}/invoices/{id}/reminders", s.invoiceToggleReminders)
	c("POST /c/{cid}/invoices/{id}/bank", s.invoiceSetBank)
	c("POST /c/{cid}/invoices/{id}/charge", s.invoiceCharge)

	c("GET /c/{cid}/recurring", s.recurringList)
	c("GET /c/{cid}/recurring/new", s.recurringForm)
	c("POST /c/{cid}/recurring/new", s.recurringSave)
	c("GET /c/{cid}/recurring/{id}", s.recurringForm)
	c("POST /c/{cid}/recurring/{id}", s.recurringSave)
	c("POST /c/{cid}/recurring/{id}/toggle", s.recurringToggle)
	c("POST /c/{cid}/recurring/{id}/delete", s.recurringDelete)
	c("POST /c/{cid}/recurring/{id}/run", s.recurringRunNow)

	ca("POST /c/{cid}/invoices/{id}/destroy", s.invoiceDestroy)
	ca("GET /c/{cid}/settings", s.companySettings)
	ca("POST /c/{cid}/settings/general", s.companySaveGeneral)
	ca("POST /c/{cid}/settings/logo", s.companySaveLogo)
	ca("POST /c/{cid}/settings/invoicing", s.companySaveInvoicing)
	ca("POST /c/{cid}/settings/bank", s.companySaveBank)
	ca("POST /c/{cid}/settings/bank/accounts", s.bankCreate)
	ca("POST /c/{cid}/settings/bank/accounts/{bid}", s.bankUpdate)
	ca("POST /c/{cid}/settings/bank/accounts/{bid}/delete", s.bankDelete)
	ca("POST /c/{cid}/settings/email", s.companySaveEmail)
	ca("POST /c/{cid}/settings/email/test", s.companyTestEmail)
	ca("POST /c/{cid}/settings/stripe", s.companySaveStripe)
	ca("POST /c/{cid}/settings/stripe/disconnect", s.companyStripeDisconnect)
	ca("POST /c/{cid}/settings/members", s.companySaveMembers)
	ca("POST /c/{cid}/settings/archive", s.companyArchive)

	return s.middleware(m)
}

func (s *Server) cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		h.ServeHTTP(w, r)
	})
}

// ---------- middleware ----------

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.isHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		r.Body = http.MaxBytesReader(sw, r.Body, 4<<20)
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "err", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				http.Error(sw, "Internal error", http.StatusInternalServerError)
			}
			if !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/healthz" {
				slog.Info("http", "method", r.Method, "path", redactPath(r.URL.Path), "status", sw.status,
					"ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// redactPath hides bearer tokens (public invoice links, invites) from logs.
func redactPath(p string) string {
	for _, pre := range []string{"/i/", "/pay/", "/card/", "/invite/", "/reset/"} {
		if strings.HasPrefix(p, pre) {
			rest := p[len(pre):]
			tail := ""
			if i := strings.Index(rest, "/"); i >= 0 {
				tail = rest[i:]
			}
			return pre + "…" + tail
		}
	}
	return p
}

// ---------- proxies, scheme and client IP ----------

func (s *Server) trusted(r *http.Request) bool {
	if s.noTrust {
		return false
	}
	if s.trustAll {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if s.trusted(r) {
		if strings.EqualFold(firstHeader(r, "X-Forwarded-Proto"), "https") {
			return true
		}
		if s.cloudflare && strings.Contains(r.Header.Get("Cf-Visitor"), `"https"`) {
			return true
		}
	}
	return false
}

func firstHeader(r *http.Request, k string) string {
	v := r.Header.Get(k)
	if i := strings.Index(v, ","); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.trusted(r) {
		return host
	}
	if s.cloudflare {
		if v := r.Header.Get("Cf-Connecting-Ip"); v != "" && net.ParseIP(v) != nil {
			return v
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// right-most address that is not one of our trusted proxies
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				continue
			}
			if !(ip.IsPrivate() || ip.IsLoopback()) || i == 0 {
				return ip.String()
			}
		}
	}
	return host
}

// detectOrigin returns the public origin (scheme://host[:port]) the browser used.
func (s *Server) detectOrigin(r *http.Request) string {
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if s.trusted(r) {
		if h := firstHeader(r, "X-Forwarded-Host"); h != "" {
			host = h
		}
	}
	host = strings.ToLower(host)
	if scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	} else {
		host = strings.TrimSuffix(host, ":80")
	}
	if !validHost(host) {
		return ""
	}
	return scheme + "://" + host
}

func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == ':' || c == '[' || c == ']') {
			return false
		}
	}
	return true
}

// sameOrigin rejects cross-site form posts (defence in depth on top of CSRF tokens).
func (s *Server) sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" || src == "null" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true // token check still applies
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	if host == strings.ToLower(r.Host) {
		return true
	}
	if s.trusted(r) && host == strings.ToLower(firstHeader(r, "X-Forwarded-Host")) {
		return true
	}
	if b, err := url.Parse(s.App.BaseURL()); err == nil && b.Host != "" && strings.EqualFold(b.Host, host) {
		return true
	}
	return false
}

// ---------- request context & auth wrappers ----------

type Ctx struct {
	W       http.ResponseWriter
	R       *http.Request
	User    *store.User
	Sess    *store.Session
	SessID  []byte
	Company *store.Company
	Lang    string
	IP      string
	RL      string // rate-limit key for the client (IPv6 grouped by /64)
	s       *Server
}

type handler func(*Ctx) error

var errForbidden = errors.New("forbidden")

const (
	cookieSession       = "inv_session"
	cookieSessionSecure = "__Host-inv_session"
	cookiePreCSRF       = "inv_csrf"
	cookieFlash         = "inv_flash"
)

func (s *Server) h(fn handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &Ctx{W: w, R: r, IP: s.clientIP(r), s: s, Lang: "en"}
		c.RL = ipKey(c.IP)
		s.loadSession(c)
		if c.User == nil {
			c.Lang = negotiateLang(r)
		}
		if r.Method == http.MethodPost {
			if !s.sameOrigin(r) || !s.checkCSRF(c) {
				s.renderError(c, http.StatusForbidden, "err.csrf")
				return
			}
		}
		if err := fn(c); err != nil {
			switch {
			case errors.Is(err, errForbidden):
				s.renderError(c, http.StatusForbidden, "err.forbidden")
			case errors.Is(err, store.ErrNotFound):
				s.renderError(c, http.StatusNotFound, "err.not_found")
			default:
				slog.Error("handler error", "path", redactPath(r.URL.Path), "err", err)
				s.renderError(c, http.StatusInternalServerError, "err.internal")
			}
		}
	}
}

func negotiateLang(r *http.Request) string {
	al := strings.ToLower(r.Header.Get("Accept-Language"))
	if strings.HasPrefix(al, "fr") {
		return "fr"
	}
	return "en"
}

func (s *Server) loadSession(c *Ctx) {
	var tok string
	names := []string{cookieSession}
	if s.isHTTPS(c.R) {
		names = []string{cookieSessionSecure} // a non-prefixed cookie could be planted by a sibling domain
	}
	for _, name := range names {
		if ck, err := c.R.Cookie(name); err == nil && ck.Value != "" {
			tok = ck.Value
			break
		}
	}
	if tok == "" || len(tok) > 100 {
		return
	}
	id := security.HashToken(tok)
	se, err := s.Store.Session(id)
	if err != nil {
		return
	}
	u, err := s.Store.User(se.UserID)
	if err != nil || u.Disabled {
		s.Store.DeleteSession(id)
		return
	}
	c.Sess, c.SessID = se, id
	if se.MFAPending {
		return // only the 2FA page can use this session
	}
	c.User = u
	c.Lang = i18n.Norm(u.Lang)
	if time.Now().Unix()-se.LastSeen > 300 {
		s.Store.TouchSession(id)
	}
}

func (s *Server) checkCSRF(c *Ctx) bool {
	if err := c.R.ParseMultipartForm(4 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		return false
	}
	got := c.R.PostFormValue("csrf")
	if got == "" {
		got = c.R.Header.Get("X-CSRF-Token")
	}
	if c.Sess != nil {
		return got != "" && security.Equal(got, c.Sess.CSRF)
	}
	ck, err := c.R.Cookie(cookiePreCSRF)
	return err == nil && got != "" && security.Equal(got, ck.Value) && c.s.validPreCSRF(ck.Value)
}

// Pre-session CSRF tokens (login, setup, invitations) are signed so that a
// cookie planted from a sibling sub-domain cannot be used for a double submit.
func (s *Server) signPreCSRF(r string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(r))
	return r + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

func (s *Server) validPreCSRF(tok string) bool {
	r, _, ok := strings.Cut(tok, ".")
	return ok && hmac.Equal([]byte(s.signPreCSRF(r)), []byte(tok))
}

// csrfToken returns the token to embed in forms.
func (c *Ctx) csrfToken() string {
	if c.Sess != nil {
		return c.Sess.CSRF
	}
	if ck, err := c.R.Cookie(cookiePreCSRF); err == nil && len(ck.Value) < 120 && c.s.validPreCSRF(ck.Value) {
		return ck.Value
	}
	tok := c.s.signPreCSRF(security.Token(18))
	http.SetCookie(c.W, &http.Cookie{Name: cookiePreCSRF, Value: tok, Path: "/", HttpOnly: true, Secure: c.s.isHTTPS(c.R),
		SameSite: http.SameSiteLaxMode, MaxAge: 3600 * 12})
	// make it visible to checkCSRF on this same request if re-rendered
	c.R.AddCookie(&http.Cookie{Name: cookiePreCSRF, Value: tok})
	return tok
}

func (s *Server) auth(fn handler) handler {
	return func(c *Ctx) error {
		if c.User == nil {
			if n, _ := s.Store.UserCount(); n == 0 {
				return c.redirect("/setup")
			}
			if c.Sess != nil && c.Sess.MFAPending {
				return c.redirect("/login/2fa")
			}
			next := c.R.URL.Path
			if c.R.Method != http.MethodGet {
				next = "/"
			}
			return c.redirect("/login?next=" + url.QueryEscape(next))
		}
		s.syncOrigin(c)
		return fn(c)
	}
}

func (s *Server) admin(fn handler) handler {
	return s.auth(func(c *Ctx) error {
		if !c.User.IsAdmin() {
			return errForbidden
		}
		return fn(c)
	})
}

func (s *Server) owner(fn handler) handler {
	return s.auth(func(c *Ctx) error {
		if !c.User.IsOwner() {
			return errForbidden
		}
		return fn(c)
	})
}

func (s *Server) requireAdmin(fn handler) handler {
	return func(c *Ctx) error {
		if !c.User.IsAdmin() {
			return errForbidden
		}
		return fn(c)
	}
}

func (s *Server) company(fn handler) handler {
	return s.auth(func(c *Ctx) error {
		id, err := strconv.ParseInt(c.R.PathValue("cid"), 10, 64)
		if err != nil {
			return store.ErrNotFound
		}
		if !s.Store.CanAccessCompany(c.User, id) {
			return errForbidden
		}
		co, err := s.Store.Company(id)
		if err != nil {
			return err
		}
		c.Company = co
		if c.Sess.CompanyID != id {
			s.Store.SetSessionCompany(c.SessID, id)
			c.Sess.CompanyID = id
		}
		return fn(c)
	})
}

// syncOrigin keeps the stored public URL up to date: it is recorded
// automatically when empty and upgraded from http to https for the same host.
// Other changes are offered to administrators as a banner.
func (s *Server) syncOrigin(c *Ctx) {
	if !c.User.IsOwner() {
		return
	}
	o := s.detectOrigin(c.R)
	if o == "" {
		return
	}
	cur := s.App.BaseURL()
	switch {
	case cur == "":
		s.Store.SetSetting("base_url", o)
		slog.Info("public URL detected", "url", o)
	case cur != o && strings.TrimPrefix(cur, "http://") == strings.TrimPrefix(o, "https://") && strings.HasPrefix(o, "https://"):
		s.Store.SetSetting("base_url", o)
		slog.Info("public URL upgraded to https", "url", o)
	}
}

// ---------- session creation ----------

func (s *Server) startSession(c *Ctx, u *store.User, mfaPending bool) error {
	if c.SessID != nil {
		s.Store.DeleteSession(c.SessID) // prevent session fixation
	}
	tok := security.Token(32)
	id := security.HashToken(tok)
	csrf := security.Token(24)
	ua := c.R.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	if err := s.Store.CreateSession(id, u.ID, csrf, mfaPending, c.IP, ua); err != nil {
		return err
	}
	name := cookieSession
	secure := s.isHTTPS(c.R)
	if secure {
		name = cookieSessionSecure
	}
	http.SetCookie(c.W, &http.Cookie{Name: name, Value: tok, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(store.SessionAbsolute.Seconds())})
	c.Sess, _ = s.Store.Session(id)
	c.SessID = id
	return nil
}

func (s *Server) clearSession(c *Ctx) {
	if c.SessID != nil {
		s.Store.DeleteSession(c.SessID)
	}
	for _, n := range []string{cookieSession, cookieSessionSecure} {
		http.SetCookie(c.W, &http.Cookie{Name: n, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: n == cookieSessionSecure})
	}
	c.Sess, c.SessID, c.User = nil, nil, nil
}

// ---------- small helpers ----------

// isLocalPath accepts only same-site absolute paths. Browsers ignore tabs and
// newlines and treat "\\" like "/", so "/\t/evil.com" would become
// "//evil.com": any control character or backslash is refused.
func isLocalPath(p string) bool {
	if p == "" || len(p) > 500 || p[0] != '/' || strings.HasPrefix(p, "//") || strings.ContainsRune(p, '\\') {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == "" && !strings.HasPrefix(u.Path, "//")
}

// noListing hides directory listings of the static file server.
func noListing(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (c *Ctx) redirect(to string) error {
	if !isLocalPath(to) {
		to = "/"
	}
	http.Redirect(c.W, c.R, to, http.StatusSeeOther)
	return nil
}

func (c *Ctx) form(k string) string { return strings.TrimSpace(c.R.PostFormValue(k)) }

func (c *Ctx) id(name string) int64 {
	v, _ := strconv.ParseInt(c.R.PathValue(name), 10, 64)
	return v
}

func (c *Ctx) cpath(format string, a ...any) string {
	return fmt.Sprintf("/c/%d", c.Company.ID) + fmt.Sprintf(format, a...)
}

func (c *Ctx) audit(action, details string) {
	var uid, cid int64
	if c.User != nil {
		uid = c.User.ID
	}
	if c.Company != nil {
		cid = c.Company.ID
	}
	c.s.Store.Audit(uid, cid, c.IP, action, details)
}

func (c *Ctx) t(key string, a ...any) string { return i18n.T(c.Lang, key, a...) }

// Shutdown helper used by main for graceful restarts.
func Shutdown(ctx context.Context, servers ...*http.Server) {
	for _, srv := range servers {
		if srv != nil {
			srv.Shutdown(ctx)
		}
	}
}
