// Package notify delivers out-of-band alerts for significant deploy events.
// The default Notifier is a no-op; the Twilio implementation sends an SMS via
// the Twilio REST API when fully configured.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event describes a service-down occurrence. The fields are intentionally
// minimal so the SMS body stays short and readable on a phone.
type Event struct {
	// BotName is the instance display name, used to identify which Nori
	// instance sent the alert when an operator runs several.
	BotName string
	// ServiceName is the configured name of the service that failed.
	ServiceName string
	// Trigger is the deploy trigger ("manual", "auto", "scheduled").
	Trigger string
	// Digest is the short image digest that was attempted (sha256:… truncated).
	Digest string
	// Reason is a one-line summary of why the deploy failed.
	Reason string
}

// Notifier sends service-down and service-recovered alerts. Implementations
// must be safe for concurrent use.
type Notifier interface {
	// NotifyServiceDown alerts that a service is down: a deploy failure or
	// a container crash detected by the monitor.
	NotifyServiceDown(ctx context.Context, evt Event) error
	// NotifyServiceRecovered alerts that a previously down service is back
	// up. It is only sent for services a down alert was sent for.
	NotifyServiceRecovered(ctx context.Context, evt Event) error
}

// Noop discards every event. It is the default when Twilio is not configured.
type Noop struct{}

func (Noop) NotifyServiceDown(context.Context, Event) error      { return nil }
func (Noop) NotifyServiceRecovered(context.Context, Event) error { return nil }

// MessageBody formats a short, SMS-friendly body for a service-down event.
// Newlines are kept so most SMS clients render a compact multi-line preview.
func MessageBody(evt Event) string {
	bot := strings.TrimSpace(evt.BotName)
	if bot == "" {
		bot = "Nori"
	}
	return fmt.Sprintf("[%s] %q deploy FAILED (trigger=%s, digest=%s): %s",
		bot, evt.ServiceName, evt.Trigger, evt.Digest, evt.Reason)
}

// RecoveredMessageBody formats a short, SMS-friendly body for a
// service-recovered event. It mirrors MessageBody but indicates recovery
// instead of failure.
func RecoveredMessageBody(evt Event) string {
	bot := strings.TrimSpace(evt.BotName)
	if bot == "" {
		bot = "Nori"
	}
	return fmt.Sprintf("[%s] %q service RECOVERED (trigger=%s, digest=%s): %s",
		bot, evt.ServiceName, evt.Trigger, evt.Digest, evt.Reason)
}

// Twilio sends SMS via the Twilio Messages REST API. Construct one with
// NewTwilio; a nil client uses http.DefaultClient.
type Twilio struct {
	BaseURL    string // defaults to "https://api.twilio.com"
	AccountSID string
	AuthToken  string
	From       string // Twilio-owned sender (E.164, e.g. "+15551234567")
	To         string // Recipient (E.164, e.g. "+15559876543")
	Client     *http.Client
}

// NewTwilio returns a Twilio notifier. Callers should pass a populated config;
// validation is the caller's responsibility (see config.TwilioConfig.Enabled).
func NewTwilio(accountSID, authToken, from, to string) *Twilio {
	return &Twilio{
		BaseURL:    "https://api.twilio.com",
		AccountSID: accountSID,
		AuthToken:  authToken,
		From:       from,
		To:         to,
		Client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// ErrMissingConfig is returned when the notifier was constructed without the
// fields Twilio requires. Callers should use config.TwilioConfig.Enabled to
// decide whether to build a Twilio notifier at all.
var ErrMissingConfig = errors.New("twilio notifier is missing required configuration")

// NotifyServiceDown POSTs the message to Twilio. It returns an error if the
// notifier is misconfigured or Twilio responds with a non-2xx status; the
// caller is expected to log and continue, never fail the deploy.
func (t *Twilio) NotifyServiceDown(ctx context.Context, evt Event) error {
	return t.send(ctx, MessageBody(evt))
}

// NotifyServiceRecovered POSTs the recovery message to Twilio, mirroring
// NotifyServiceDown with the recovery body.
func (t *Twilio) NotifyServiceRecovered(ctx context.Context, evt Event) error {
	return t.send(ctx, RecoveredMessageBody(evt))
}

// send POSTs a preformatted SMS body to the Twilio Messages API.
func (t *Twilio) send(ctx context.Context, body string) error {
	if t == nil || t.AccountSID == "" || t.AuthToken == "" || t.From == "" || t.To == "" {
		return ErrMissingConfig
	}
	base := t.BaseURL
	if base == "" {
		base = "https://api.twilio.com"
	}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json",
		strings.TrimRight(base, "/"), url.PathEscape(t.AccountSID))

	form := url.Values{}
	form.Set("From", t.From)
	form.Set("To", t.To)
	form.Set("Body", body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: build request: %w", err)
	}
	req.SetBasicAuth(t.AccountSID, t.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Twilio returns JSON error detail; surface a useful slice without
		// swallowing the whole body.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		var twErr struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		}
		_ = json.Unmarshal(body, &twErr)
		detail := strings.TrimSpace(twErr.Message)
		if detail == "" {
			detail = strings.TrimSpace(string(body))
		}
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("twilio: %s: %s (code %d)", resp.Status, detail, twErr.Code)
	}
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// LogFailures wraps a Notifier so that send failures are logged but never
// propagated. Use this around the notifier at the call site so a Twilio
// outage can never fail a deploy.
type LogFailures struct {
	Inner Notifier
}

func (l *LogFailures) NotifyServiceDown(ctx context.Context, evt Event) error {
	err := l.Inner.NotifyServiceDown(ctx, evt)
	if err != nil {
		log.Printf("notify: service-down alert for %q failed: %v", evt.ServiceName, err)
	}
	return nil
}

func (l *LogFailures) NotifyServiceRecovered(ctx context.Context, evt Event) error {
	err := l.Inner.NotifyServiceRecovered(ctx, evt)
	if err != nil {
		log.Printf("notify: service-recovered alert for %q failed: %v", evt.ServiceName, err)
	}
	return nil
}
