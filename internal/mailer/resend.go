// Package mailer sends e-mails through each company's own Resend account.
package mailer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

type Attachment struct {
	Filename string
	Content  []byte
}

type Message struct {
	From           string
	To             []string
	BCC            []string
	ReplyTo        string
	Subject        string
	HTML           string
	Text           string
	Attachments    []Attachment
	IdempotencyKey string
}

type apiError struct {
	Message string `json:"message"`
	Name    string `json:"name"`
}

// Send delivers the message and returns the Resend e-mail id.
func Send(ctx context.Context, apiKey string, m Message) (string, error) {
	if apiKey == "" {
		return "", errors.New("Resend is not configured for this company")
	}
	if len(m.To) == 0 {
		return "", errors.New("no recipient")
	}
	body := map[string]any{
		"from":    m.From,
		"to":      m.To,
		"subject": m.Subject,
		"html":    m.HTML,
		"text":    m.Text,
	}
	if m.ReplyTo != "" {
		body["reply_to"] = m.ReplyTo
	}
	if len(m.BCC) > 0 {
		body["bcc"] = m.BCC
	}
	if len(m.Attachments) > 0 {
		var atts []map[string]string
		for _, a := range m.Attachments {
			atts = append(atts, map[string]string{"filename": a.Filename, "content": base64.StdEncoding.EncodeToString(a.Content)})
		}
		body["attachments"] = atts
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Invoicer")
	if m.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e apiError
		json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return "", fmt.Errorf("resend: %s (HTTP %d)", e.Message, resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &out)
	return out.ID, nil
}

// CheckKey validates an API key by listing domains (works with full-access
// keys; a sending-only key returns 401 "restricted_api_key" which we accept).
func CheckKey(ctx context.Context, apiKey string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.resend.com/domains", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "Invoicer")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 200 {
		return nil
	}
	var e apiError
	json.Unmarshal(raw, &e)
	if e.Name == "restricted_api_key" {
		return nil
	}
	if e.Message == "" {
		e.Message = resp.Status
	}
	return fmt.Errorf("resend: %s", e.Message)
}
