package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flocom/invoicer/internal/backup"
	"github.com/flocom/invoicer/internal/store"
)

// postFile sends a multipart form with one file.
func (b *browser) postFile(path string, fields url.Values, file []byte) {
	b.e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if fields.Get("csrf") == "" {
		fields.Set("csrf", b.csrf)
	}
	for k, vs := range fields {
		for _, v := range vs {
			w.WriteField(k, v)
		}
	}
	fw, _ := w.CreateFormFile("file", "x.invbak")
	fw.Write(file)
	w.Close()
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", b.e.srv.URL)
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	b.status, b.last = resp.StatusCode, string(raw)
	if m := csrfRe.FindStringSubmatch(b.last); m != nil {
		b.csrf = m[1]
	}
}

func TestFullBackupRestoreAndMove(t *testing.T) {
	old := newEnv(t)
	b := setupOwner(t, old)
	b.post("/companies/new", url.Values{"name": {"Alpha"}, "currency": {"EUR"}, "lang": {"en"}})
	b.post("/c/1/clients/new", url.Values{"name": {"Globex"}, "lang": {"en"}})
	due := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	b.post("/c/1/invoices/new", url.Values{"client_id": {"1"}, "currency": {"EUR"}, "lang": {"en"}, "issue_date": {old.app.Today()},
		"due_date": {due}, "line_desc": {"Work"}, "line_qty": {"1"}, "line_price": {"120"}, "line_tax": {"0"}, "action": {"issue"}})
	inv, _ := old.app.Store.Invoice(1, 1)

	b.get("/admin/system")
	b.must("Full backup")
	// the account password and matching passphrases are required
	b.post("/admin/system/full-backup", url.Values{"current": {"wrong password!!"}, "passphrase": {"moving day 2026"}, "passphrase2": {"moving day 2026"}})
	b.must("current password is incorrect")
	b.post("/admin/system/full-backup", url.Values{"current": {pw}, "passphrase": {"moving day 2026"}, "passphrase2": {"other"}})
	if strings.HasPrefix(b.last, "INVOICER-BACKUP") {
		t.Fatal("backup made with mismatched passphrases")
	}
	b.post("/admin/system/full-backup", url.Values{"current": {pw}, "passphrase": {"moving day 2026"}, "passphrase2": {"moving day 2026"}})
	if !strings.HasPrefix(b.last, "INVOICER-BACKUP\n") {
		t.Fatalf("no backup file: %d %.200s", b.status, b.last)
	}
	file := []byte(b.last)

	// a brand new server offers to restore instead of creating an account
	fresh := newEnv(t)
	nb := fresh.browser()
	nb.get("/setup")
	nb.must("/setup/restore")
	nb.get("/setup/restore")
	nb.postFile("/setup/restore", url.Values{"passphrase": {"not the passphrase"}}, file)
	nb.must("Wrong passphrase")
	nb.postFile("/setup/restore", url.Values{"passphrase": {"moving day 2026"}, "move_webhooks": {"1"}}, file)
	nb.must("Restoring")
	nb.must("Sign in with the e-mail and password you used on the old server")
	if !backup.Pending(fresh.app.Cfg.DataDir) {
		t.Fatal("restore not staged")
	}
	staged, err := store.Open(filepath.Join(fresh.app.Cfg.DataDir, "restore", "invoicer.db"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := staged.Invoice(1, 1); err != nil || got.Number != inv.Number {
		t.Fatalf("restored invoice: %v %v", got, err)
	}
	if staged.Setting("stripe_webhooks_hold") != "" || staged.Setting("stripe_webhooks_refresh") != "1" {
		t.Fatal("the restored server should take the Stripe webhooks over")
	}
	if staged.Setting("base_url") != fresh.srv.URL {
		t.Fatalf("the restored data should use the new address: %q", staged.Setting("base_url"))
	}
	staged.Close()

	// once an account exists, the restore page is closed to visitors
	setupOwner(t, fresh)
	anon := fresh.browser()
	anon.get("/setup/restore")
	anon.must("Sign in")

	// the old server, once moved, redirects its public links and stops invoicing
	b.post("/admin/system/moved", url.Values{"current": {pw}, "moved_to": {"https://invoices.new.example"}})
	if old.app.MovedTo() != "https://invoices.new.example" {
		t.Fatalf("moved to: %q", old.app.MovedTo())
	}
	b.get("/c/1")
	b.must("This server has moved to https://invoices.new.example")
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(old.srv.URL + "/i/" + inv.PublicToken + "?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://invoices.new.example/i/"+inv.PublicToken+"?x=1" {
		t.Fatalf("public link: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	b.post("/admin/system/moved", url.Values{"current": {pw}, "clear": {"1"}})
	if old.app.MovedTo() != "" {
		t.Fatal("the move should be cancelled")
	}

	// restoring over an existing server needs the password and the confirmation
	old.web.limiter = newLimiter() // password attempts are limited to 5 per 10 minutes
	b.postFile("/admin/system/restore", url.Values{"current": {pw}, "passphrase": {"moving day 2026"}}, file)
	b.must("Tick the box")
	b.postFile("/admin/system/restore", url.Values{"current": {pw}, "passphrase": {"moving day 2026"}, "confirm": {"1"}}, file)
	b.must("Restoring")
}
