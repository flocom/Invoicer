// Package stripe is a minimal client for the parts of the Stripe API Invoicer
// uses: Checkout Sessions, webhook endpoints and webhook signature checks.
package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("stripe: %s (HTTP %d)", e.Message, e.Status) }

func call(ctx context.Context, key, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.com"+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(key, "")
	req.Header.Set("Stripe-Version", "2024-06-20")
	req.Header.Set("User-Agent", "Invoicer")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error.Message == "" {
			e.Error.Message = resp.Status
		}
		return &Error{Status: resp.StatusCode, Message: e.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// ValidKeyFormat accepts secret and restricted keys only (never publishable).
func ValidKeyFormat(k string) bool {
	for _, p := range []string{"sk_live_", "sk_test_", "rk_live_", "rk_test_"} {
		if strings.HasPrefix(k, p) && len(k) > 20 {
			return true
		}
	}
	return false
}

func IsTestKey(k string) bool { return strings.Contains(k, "_test_") }

// AccountName returns a human label for the account behind the key.
func AccountName(ctx context.Context, key string) (string, error) {
	var acc struct {
		ID              string `json:"id"`
		BusinessProfile struct {
			Name string `json:"name"`
		} `json:"business_profile"`
		Settings struct {
			Dashboard struct {
				DisplayName string `json:"display_name"`
			} `json:"dashboard"`
		} `json:"settings"`
		Email string `json:"email"`
	}
	if err := call(ctx, key, http.MethodGet, "/v1/account", nil, &acc); err != nil {
		var se *Error
		// Restricted keys may not read the account; probe Checkout instead.
		if errors.As(err, &se) && (se.Status == 403 || se.Status == 401) && strings.HasPrefix(key, "rk_") {
			if err2 := call(ctx, key, http.MethodGet, "/v1/checkout/sessions?limit=1", nil, nil); err2 != nil {
				return "", err2
			}
			return "Stripe account", nil
		}
		return "", err
	}
	name := acc.Settings.Dashboard.DisplayName
	if name == "" {
		name = acc.BusinessProfile.Name
	}
	if name == "" {
		name = acc.Email
	}
	if name == "" {
		name = acc.ID
	}
	return name, nil
}

var webhookEvents = []string{
	"checkout.session.completed",
	"checkout.session.async_payment_succeeded",
	"checkout.session.async_payment_failed",
	"checkout.session.expired",
}

// CreateWebhook registers our endpoint on the account and returns its id and
// signing secret.
func CreateWebhook(ctx context.Context, key, endpoint string) (id, secret string, err error) {
	f := url.Values{}
	f.Set("url", endpoint)
	f.Set("description", "Invoicer (automatic)")
	for _, e := range webhookEvents {
		f.Add("enabled_events[]", e)
	}
	var out struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := call(ctx, key, http.MethodPost, "/v1/webhook_endpoints", f, &out); err != nil {
		return "", "", err
	}
	return out.ID, out.Secret, nil
}

func DeleteWebhook(ctx context.Context, key, id string) error {
	if id == "" {
		return nil
	}
	return call(ctx, key, http.MethodDelete, "/v1/webhook_endpoints/"+url.PathEscape(id), nil, nil)
}

type CheckoutParams struct {
	Amount      int64
	Currency    string
	ProductName string
	Description string
	Email       string
	SuccessURL  string
	CancelURL   string
	Locale      string
	Metadata    map[string]string
}

type Session struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Status        string            `json:"status"`         // open, complete, expired
	PaymentStatus string            `json:"payment_status"` // paid, unpaid, no_payment_required
	AmountTotal   int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
	ExpiresAt     int64             `json:"expires_at"`
	Metadata      map[string]string `json:"metadata"`
	PaymentIntent string            `json:"payment_intent"`
}

func CreateCheckout(ctx context.Context, key string, p CheckoutParams) (*Session, error) {
	f := url.Values{}
	f.Set("mode", "payment")
	f.Set("line_items[0][quantity]", "1")
	f.Set("line_items[0][price_data][currency]", strings.ToLower(p.Currency))
	f.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(p.Amount, 10))
	f.Set("line_items[0][price_data][product_data][name]", p.ProductName)
	if p.Description != "" {
		f.Set("line_items[0][price_data][product_data][description]", p.Description)
	}
	f.Set("success_url", p.SuccessURL)
	f.Set("cancel_url", p.CancelURL)
	if p.Email != "" {
		f.Set("customer_email", p.Email)
	}
	if p.Locale != "" {
		f.Set("locale", p.Locale)
	}
	f.Set("expires_at", strconv.FormatInt(time.Now().Add(23*time.Hour).Unix(), 10))
	for k, v := range p.Metadata {
		f.Set("metadata["+k+"]", v)
		f.Set("payment_intent_data[metadata]["+k+"]", v)
	}
	f.Set("payment_intent_data[description]", p.ProductName)
	var s Session
	if err := call(ctx, key, http.MethodPost, "/v1/checkout/sessions", f, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func GetCheckout(ctx context.Context, key, id string) (*Session, error) {
	var s Session
	if err := call(ctx, key, http.MethodGet, "/v1/checkout/sessions/"+url.PathEscape(id), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Event is the subset of a webhook event we need.
type Event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// VerifyWebhook checks the Stripe-Signature header (HMAC-SHA256 over
// "timestamp.payload") with a 5 minute tolerance and parses the event.
func VerifyWebhook(payload []byte, header, secret string, now time.Time) (*Event, error) {
	if secret == "" {
		return nil, errors.New("no webhook secret")
	}
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return nil, errors.New("malformed signature header")
	}
	if d := now.Unix() - ts; d > 300 || d < -300 {
		return nil, errors.New("signature timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	want := mac.Sum(nil)
	ok := false
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err == nil && hmac.Equal(got, want) {
			ok = true
		}
	}
	if !ok {
		return nil, errors.New("invalid signature")
	}
	var ev Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}
