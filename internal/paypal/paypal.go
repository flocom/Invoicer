// Package paypal is a minimal client for the PayPal REST API (Orders v2):
// the client approves an order on PayPal's own page — with their PayPal
// account or with a bank card — and the order is then captured.
package paypal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

// API roots (tests point them to a fake server).
var (
	LiveBase    = "https://api-m.paypal.com"
	SandboxBase = "https://api-m.sandbox.paypal.com"
)

// Credentials of a PayPal REST app (developer.paypal.com → Apps & Credentials).
type Credentials struct {
	ClientID string
	Secret   string
	Sandbox  bool
}

func (c Credentials) base() string {
	if c.Sandbox {
		return SandboxBase
	}
	return LiveBase
}

type Error struct {
	Status  int
	Name    string // e.g. UNPROCESSABLE_ENTITY, INVALID_CLIENT
	Issue   string // e.g. ORDER_ALREADY_CAPTURED, INSTRUMENT_DECLINED
	Message string
}

func (e *Error) Error() string {
	msg := e.Message
	if e.Issue != "" {
		msg += " (" + e.Issue + ")"
	}
	return fmt.Sprintf("paypal: %s (HTTP %d)", msg, e.Status)
}

// HasIssue tells an API error carrying this issue code.
func HasIssue(err error, issue string) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Issue == issue
}

// ---------- access tokens ----------

type token struct {
	value   string
	expires time.Time
}

var (
	tokensMu sync.Mutex
	tokens   = map[[32]byte]token{}
)

func tokenKey(c Credentials) [32]byte {
	return sha256.Sum256([]byte(c.base() + "\x00" + c.ClientID + "\x00" + c.Secret))
}

func accessToken(ctx context.Context, c Credentials) (string, error) {
	k := tokenKey(c)
	tokensMu.Lock()
	t, ok := tokens[k]
	tokensMu.Unlock()
	if ok && time.Until(t.expires) > time.Minute {
		return t.value, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/v1/oauth2/token",
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.ClientID, c.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := do(req, &out); err != nil {
		var pe *Error
		if errors.As(err, &pe) && (pe.Status == 401 || pe.Status == 400) {
			return "", errors.New("PayPal refused these credentials (check the client ID, the secret and live/sandbox mode)")
		}
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("paypal: no access token")
	}
	tokensMu.Lock()
	tokens[k] = token{value: out.AccessToken, expires: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}
	tokensMu.Unlock()
	return out.AccessToken, nil
}

// Verify checks the credentials by getting an access token.
func Verify(ctx context.Context, c Credentials) error {
	_, err := accessToken(ctx, c)
	return err
}

func do(req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Name             string `json:"name"`
			Message          string `json:"message"`
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			Details          []struct {
				Issue       string `json:"issue"`
				Description string `json:"description"`
			} `json:"details"`
		}
		json.Unmarshal(raw, &e)
		pe := &Error{Status: resp.StatusCode, Name: firstNonEmpty(e.Name, e.Error), Message: firstNonEmpty(e.Message, e.ErrorDescription, resp.Status)}
		if len(e.Details) > 0 {
			pe.Issue = e.Details[0].Issue
			if e.Details[0].Description != "" {
				pe.Message = e.Details[0].Description
			}
		}
		return pe
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func call(ctx context.Context, c Credentials, method, path string, body any, requestID string, out any) error {
	tok, err := accessToken(ctx, c)
	if err != nil {
		return err
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if requestID != "" {
		req.Header.Set("PayPal-Request-Id", requestID) // idempotency
	}
	return do(req, out)
}

// ---------- orders ----------

// Order is the subset of an order Invoicer needs.
type Order struct {
	ID            string `json:"id"`
	Status        string `json:"status"` // CREATED, PAYER_ACTION_REQUIRED, APPROVED, COMPLETED, VOIDED
	PurchaseUnits []struct {
		CustomID string `json:"custom_id"`
		Amount   struct {
			CurrencyCode string `json:"currency_code"`
			Value        string `json:"value"`
		} `json:"amount"`
		Payments struct {
			Captures []Capture `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
	Links []struct {
		Href string `json:"href"`
		Rel  string `json:"rel"`
	} `json:"links"`
}

type Capture struct {
	ID     string `json:"id"`
	Status string `json:"status"` // COMPLETED, PENDING, DECLINED…
	Amount struct {
		CurrencyCode string `json:"currency_code"`
		Value        string `json:"value"`
	} `json:"amount"`
}

// ApproveURL is the PayPal page where the client pays.
func (o *Order) ApproveURL() string {
	for _, l := range o.Links {
		if l.Rel == "payer-action" || l.Rel == "approve" {
			return l.Href
		}
	}
	return ""
}

// Captured returns the completed capture of the order, if any.
func (o *Order) Captured() *Capture {
	for _, u := range o.PurchaseUnits {
		for i := range u.Payments.Captures {
			if c := &u.Payments.Captures[i]; c.Status == "COMPLETED" {
				return c
			}
		}
	}
	return nil
}

// Pending tells a capture waiting for PayPal (e.g. under review).
func (o *Order) Pending() bool {
	for _, u := range o.PurchaseUnits {
		for _, c := range u.Payments.Captures {
			if c.Status == "PENDING" {
				return true
			}
		}
	}
	return false
}

type OrderParams struct {
	Amount      int64  // in cents
	Currency    string // EUR, USD…
	Description string
	CustomID    string // our reference, returned with the order
	BrandName   string
	Locale      string // fr-FR, en-US
	Card        bool   // open PayPal's page on the card form (no PayPal account needed)
	ReturnURL   string
	CancelURL   string
	RequestID   string
}

// CreateOrder creates an order the client approves on PayPal's page.
func CreateOrder(ctx context.Context, c Credentials, p OrderParams) (*Order, error) {
	landing := "LOGIN"
	if p.Card {
		landing = "GUEST_CHECKOUT"
	}
	body := map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{{
			"custom_id":   p.CustomID,
			"description": truncate(p.Description, 127),
			"amount":      map[string]string{"currency_code": strings.ToUpper(p.Currency), "value": FormatAmount(p.Amount)},
		}},
		"payment_source": map[string]any{"paypal": map[string]any{"experience_context": map[string]string{
			"brand_name":          truncate(p.BrandName, 127),
			"locale":              p.Locale,
			"landing_page":        landing,
			"user_action":         "PAY_NOW",
			"shipping_preference": "NO_SHIPPING",
			"return_url":          p.ReturnURL,
			"cancel_url":          p.CancelURL,
		}}},
	}
	var o Order
	if err := call(ctx, c, http.MethodPost, "/v2/checkout/orders", body, p.RequestID, &o); err != nil {
		return nil, err
	}
	if o.ApproveURL() == "" {
		return nil, errors.New("paypal: the order has no payment page")
	}
	return &o, nil
}

func GetOrder(ctx context.Context, c Credentials, id string) (*Order, error) {
	var o Order
	if err := call(ctx, c, http.MethodGet, "/v2/checkout/orders/"+url.PathEscape(id), nil, "", &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// CaptureOrder takes the money of an approved order. Capturing twice is
// harmless: the second call returns the order as it is.
func CaptureOrder(ctx context.Context, c Credentials, id string) (*Order, error) {
	var o Order
	err := call(ctx, c, http.MethodPost, "/v2/checkout/orders/"+url.PathEscape(id)+"/capture", map[string]any{}, "capture-"+id, &o)
	if HasIssue(err, "ORDER_ALREADY_CAPTURED") {
		return GetOrder(ctx, c, id)
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// FormatAmount writes cents as PayPal expects ("123.45").
func FormatAmount(cents int64) string {
	return strconv.FormatInt(cents/100, 10) + "." + fmt.Sprintf("%02d", cents%100)
}

// ParseAmount reads "123.45" (or "123") as cents.
func ParseAmount(v string) (int64, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(v), ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("paypal: bad amount %q", v)
	}
	frac = (frac + "00")[:2]
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w < 0 {
		return 0, fmt.Errorf("paypal: bad amount %q", v)
	}
	return w*100 + f, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------- environment detection ----------

// Detect finds whether the credentials belong to a live or a sandbox app.
func Detect(ctx context.Context, clientID, secret string) (Credentials, error) {
	live := Credentials{ClientID: clientID, Secret: secret}
	errLive := Verify(ctx, live)
	if errLive == nil {
		return live, nil
	}
	sandbox := Credentials{ClientID: clientID, Secret: secret, Sandbox: true}
	if Verify(ctx, sandbox) == nil {
		return sandbox, nil
	}
	return live, errLive
}

// ---------- webhooks ----------

// WebhookEvents are the events Invoicer listens to.
var WebhookEvents = []string{"CHECKOUT.ORDER.APPROVED", "PAYMENT.CAPTURE.COMPLETED"}

type Webhook struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateWebhook registers our endpoint on the app (reusing one already
// registered for that URL) and returns its id.
func CreateWebhook(ctx context.Context, c Credentials, endpoint string) (string, error) {
	var types []map[string]string
	for _, e := range WebhookEvents {
		types = append(types, map[string]string{"name": e})
	}
	var w Webhook
	err := call(ctx, c, http.MethodPost, "/v1/notifications/webhooks", map[string]any{"url": endpoint, "event_types": types}, "", &w)
	if HasIssue(err, "WEBHOOK_URL_ALREADY_EXISTS") || isName(err, "WEBHOOK_URL_ALREADY_EXISTS") {
		hooks, lerr := ListWebhooks(ctx, c)
		if lerr != nil {
			return "", err
		}
		for _, h := range hooks {
			if h.URL == endpoint {
				return h.ID, nil
			}
		}
	}
	if err != nil {
		return "", err
	}
	return w.ID, nil
}

func isName(err error, name string) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Name == name
}

func ListWebhooks(ctx context.Context, c Credentials) ([]Webhook, error) {
	var out struct {
		Webhooks []Webhook `json:"webhooks"`
	}
	if err := call(ctx, c, http.MethodGet, "/v1/notifications/webhooks", nil, "", &out); err != nil {
		return nil, err
	}
	return out.Webhooks, nil
}

// DeleteWebhook removes an endpoint; one already gone is not an error.
func DeleteWebhook(ctx context.Context, c Credentials, id string) error {
	if id == "" {
		return nil
	}
	err := call(ctx, c, http.MethodDelete, "/v1/notifications/webhooks/"+url.PathEscape(id), nil, "", nil)
	var pe *Error
	if errors.As(err, &pe) && pe.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// VerifyWebhook asks PayPal whether a received event really comes from it.
func VerifyWebhook(ctx context.Context, c Credentials, webhookID string, h http.Header, body []byte) error {
	if webhookID == "" {
		return errors.New("no webhook registered")
	}
	req := map[string]any{
		"auth_algo":         h.Get("Paypal-Auth-Algo"),
		"cert_url":          h.Get("Paypal-Cert-Url"),
		"transmission_id":   h.Get("Paypal-Transmission-Id"),
		"transmission_sig":  h.Get("Paypal-Transmission-Sig"),
		"transmission_time": h.Get("Paypal-Transmission-Time"),
		"webhook_id":        webhookID,
		"webhook_event":     json.RawMessage(body),
	}
	var out struct {
		Status string `json:"verification_status"`
	}
	if err := call(ctx, c, http.MethodPost, "/v1/notifications/verify-webhook-signature", req, "", &out); err != nil {
		return err
	}
	if out.Status != "SUCCESS" {
		return errors.New("paypal: webhook signature not verified")
	}
	return nil
}

// Event is the subset of a webhook event we need.
type Event struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	Resource  struct {
		ID                string `json:"id"`
		SupplementaryData struct {
			RelatedIDs struct {
				OrderID string `json:"order_id"`
			} `json:"related_ids"`
		} `json:"supplementary_data"`
	} `json:"resource"`
}

// OrderID returns the order an event is about.
func (e *Event) OrderID() string {
	if e.EventType == "CHECKOUT.ORDER.APPROVED" {
		return e.Resource.ID
	}
	return e.Resource.SupplementaryData.RelatedIDs.OrderID
}
