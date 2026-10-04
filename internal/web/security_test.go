package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/security"
	"github.com/flocom/invoicer/internal/store"
)

// post with a spoofable client address (the test server peer is loopback,
// which is trusted as a proxy).
func (b *browser) postFrom(ip, path string, form url.Values) *http.Response {
	b.e.t.Helper()
	form.Set("csrf", b.csrf)
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.e.srv.URL)
	req.Header.Set("X-Forwarded-For", ip)
	c := *b.c
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestIsLocalPath(t *testing.T) {
	good := []string{"/", "/c/1/invoices", "/c/1?x=1&y=2"}
	bad := []string{"", "//evil.com", "/\\evil.com", "/\t/evil.com", "/\n/evil.com", "https://evil.com", "evil.com", "/%09/x\x7f"}
	for _, p := range good {
		if !isLocalPath(p) {
			t.Errorf("%q should be accepted", p)
		}
	}
	for _, p := range bad {
		if isLocalPath(p) {
			t.Errorf("%q must be rejected", p)
		}
	}
}

func TestLoginRedirectAndThrottling(t *testing.T) {
	e := newEnv(t)
	setupOwner(t, e)

	// open redirect: a tab-prefixed next must land on "/"
	b := e.browser()
	b.get("/login")
	resp := b.postFrom("203.0.113.1", "/login", url.Values{"email": {"owner@example.test"}, "password": {pw}, "next": {"/\t/evil.com"}})
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("unsafe redirect to %q", loc)
	}

	// an attacker exhausting the failure budget from one address…
	a := e.browser()
	a.get("/login")
	for i := 0; i < 6; i++ {
		a.postFrom("203.0.113.66", "/login", url.Values{"email": {"owner@example.test"}, "password": {"wrong password!!"}})
	}
	if r := a.postFrom("203.0.113.66", "/login", url.Values{"email": {"owner@example.test"}, "password": {pw}}); r.StatusCode != 429 {
		t.Fatalf("attacker should be throttled, got %d", r.StatusCode)
	}
	// …cannot lock the owner out from another address
	o := e.browser()
	o.get("/login")
	if r := o.postFrom("198.51.100.7", "/login", url.Values{"email": {"owner@example.test"}, "password": {pw}}); r.StatusCode != 303 {
		t.Fatalf("owner should still log in, got %d", r.StatusCode)
	}
	// unknown accounts get the same answers (no enumeration)
	u := e.browser()
	u.get("/login")
	for i := 0; i < 6; i++ {
		u.postFrom("203.0.113.77", "/login", url.Values{"email": {"nobody@example.test"}, "password": {"wrong password!!"}})
	}
	if r := u.postFrom("203.0.113.77", "/login", url.Values{"email": {"nobody@example.test"}, "password": {"x"}}); r.StatusCode != 429 {
		t.Fatalf("unknown account should look the same, got %d", r.StatusCode)
	}
}

func TestLimiterBoundedKeys(t *testing.T) {
	l := newLimiter()
	big := strings.Repeat("x", 4<<20)
	for i := 0; i < 3; i++ {
		l.allow(big+string(rune('a'+i)), 5, time.Minute)
	}
	if len(l.buckets) != 3 {
		t.Fatal("unexpected bucket count")
	}
	for k := range l.buckets {
		if len(k) != 16 {
			t.Fatal("keys must be fixed-size hashes")
		}
	}
	if ipKey("2001:db8:1:2:3:4:5:6") != ipKey("2001:db8:1:2:ffff::1") {
		t.Fatal("IPv6 clients must be grouped by /64")
	}
}

func TestProxyHeaders(t *testing.T) {
	e := newEnv(t)
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.17.0.1:1234"
	r.Header.Set("Cf-Connecting-Ip", "1.2.3.4")
	r.Header.Set("X-Real-Ip", "5.6.7.8")
	if ip := e.web.clientIP(r); ip != "172.17.0.1" {
		t.Fatalf("CF/X-Real-IP must be ignored by default, got %s", ip)
	}
}

func TestForgedFlashAndCookieScope(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	// unsigned flash cookie is ignored
	req, _ := http.NewRequest("GET", e.srv.URL+"/login", nil)
	req.AddCookie(&http.Cookie{Name: cookieFlash, Value: "eyJrIjoiZXJyIiwibSI6IlBXTkVEIn0"})
	resp, _ := http.DefaultClient.Do(req)
	buf := make([]byte, 64<<10)
	n, _ := resp.Body.Read(buf)
	resp.Body.Close()
	if strings.Contains(string(buf[:n]), "PWNED") {
		t.Fatal("forged flash message rendered")
	}
	// on HTTPS a non-__Host- session cookie is not accepted
	var sess string
	u, _ := url.Parse(e.srv.URL)
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == cookieSession {
			sess = c.Value
		}
	}
	req, _ = http.NewRequest("GET", e.srv.URL+"/account", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sess})
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ = c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("plain session cookie must be refused over HTTPS, got %d", resp.StatusCode)
	}
}

func TestResetLinksInvalidatedAndDomainOwnerOnly(t *testing.T) {
	e := newEnv(t)
	owner := setupOwner(t, e)
	st := e.app.Store
	m, _ := st.CreateUser("m@example.test", "M", security.HashPassword(pw), store.RoleMember, "en")
	tok := security.HashToken("reset-token")
	st.CreatePasswordReset(tok, m.ID, time.Hour)
	st.SetUserRole(m.ID, store.RoleAdmin)
	if _, err := st.PasswordResetUser(tok); err == nil {
		t.Fatal("reset link must die when the role changes")
	}

	// a non-owner admin cannot repoint the public URL
	adm := e.browser()
	adm.get("/login")
	adm.post("/login", url.Values{"email": {"m@example.test"}, "password": {pw}})
	adm.get("/account")
	adm.post("/admin/system/domain", url.Values{})
	if adm.status != 403 {
		t.Fatalf("admin must not change the public URL, got %d", adm.status)
	}
	// the owner needs the password
	before := e.app.BaseURL()
	owner.get("/admin/system")
	owner.post("/admin/system/domain", url.Values{"current": {"wrong"}})
	if e.app.BaseURL() != before {
		t.Fatal("public URL changed without password")
	}
}

func TestMFADisableNeedsCode(t *testing.T) {
	e := newEnv(t)
	b := setupOwner(t, e)
	u, _ := e.app.Store.UserByEmail("owner@example.test")
	secret := security.NewTOTPSecret()
	e.app.Store.SetTOTP(u.ID, e.app.Box.Seal(secret, totpAAD(u.ID)), true, "[]")
	b.get("/account")
	b.post("/account/2fa/disable", url.Values{"current": {pw}, "code": {"000000"}})
	if u2, _ := e.app.Store.User(u.ID); !u2.TOTPEnabled {
		t.Fatal("2FA disabled without a valid code")
	}
	b.get("/account")
	b.post("/account/2fa/disable", url.Values{"current": {pw}, "code": {security.TOTPCode(secret, time.Now())}})
	if u2, _ := e.app.Store.User(u.ID); u2.TOTPEnabled {
		t.Fatal("2FA should be disabled with password + code")
	}
}

func TestAmountLimits(t *testing.T) {
	if !withinLimits(1000, 100) || withinLimits(1_000_000_000, 100_000_000_000) || withinLimits(1e10, 1) {
		t.Fatal("bounds")
	}
}
