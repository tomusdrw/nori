// Package notify delivers out-of-band alerts for significant deploy events.
// The default Notifier is a no-op; the Twilio implementation sends an SMS via
// the Twilio REST API and the Telegram implementation posts via the Telegram
// Bot API when fully configured. Several notifiers can run side by side
// through Multi.
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

	"nori/internal/store"
)

// Event describes a deployment or service-health notification. The fields are
// intentionally minimal so the SMS body stays short and readable on a phone.
type Event struct {
	// BotName is the instance display name, used to identify which Nori
	// instance sent the alert when an operator runs several.
	BotName string
	// ServiceName is the configured name of the affected service.
	ServiceName string
	// DeploymentID identifies the deployment detail page for deploy events.
	// It is zero for monitor-only service health events.
	DeploymentID int64
	// Trigger is the deploy trigger ("manual", "auto", "scheduled") or
	// "monitor" for service health events.
	Trigger string
	// Digest is the short image digest that was attempted (sha256:… truncated).
	Digest string
	// Reason is a one-line summary of why the deploy or health check failed.
	Reason string
}

// Notifier sends deployment and service-health alerts. Implementations
// must be safe for concurrent use.
type Notifier interface {
	// NotifyServiceDown alerts that a service is down: a deploy failure or
	// a container crash detected by the monitor.
	NotifyServiceDown(ctx context.Context, evt Event) error
	// NotifyServiceRecovered alerts that a previously down service is back
	// up. It is only sent for services a down alert was sent for.
	NotifyServiceRecovered(ctx context.Context, evt Event) error
	// NotifyDeploySuccess reports a successful deployment.
	NotifyDeploySuccess(ctx context.Context, evt Event) error
}

// Noop discards every event. It is the default when no notifier is configured.
type Noop struct{}

func (Noop) NotifyServiceDown(context.Context, Event) error      { return nil }
func (Noop) NotifyServiceRecovered(context.Context, Event) error { return nil }
func (Noop) NotifyDeploySuccess(context.Context, Event) error    { return nil }

// MessageBody formats a short, SMS-friendly body for a service-down event.
// Newlines are kept so most SMS clients render a compact multi-line preview.
func MessageBody(evt Event) string {
	icon, title := "❌", "Deploy failed"
	metadata := make([]string, 0, 2)
	if evt.Trigger == store.TriggerMonitor {
		icon, title = "🚨", "Service down"
	} else if evt.Trigger != "" {
		metadata = append(metadata, evt.Trigger)
	}
	if evt.Digest != "" {
		metadata = append(metadata, evt.Digest)
	}
	detail := strings.Join(metadata, " · ")
	if evt.Reason != "" {
		if detail != "" {
			detail += " — "
		}
		detail += evt.Reason
	}
	return smsMessage(icon, title, evt, detail)
}

// RecoveredMessageBody formats a short, SMS-friendly body for a
// service-recovered event.
func RecoveredMessageBody(evt Event) string {
	return smsMessage("✅", "Service recovered", evt, evt.Digest)
}

// SuccessMessageBody formats a short body for a successful deployment.
// No reason is included; a successful deploy has nothing to explain.
func SuccessMessageBody(evt Event) string {
	metadata := make([]string, 0, 2)
	if evt.Trigger != "" {
		metadata = append(metadata, evt.Trigger)
	}
	if evt.Digest != "" {
		metadata = append(metadata, evt.Digest)
	}
	return smsMessage("🚀", "Deployed", evt, strings.Join(metadata, " · "))
}

func smsMessage(icon, title string, evt Event, detail string) string {
	bot := strings.TrimSpace(evt.BotName)
	if bot == "" {
		bot = "Nori"
	}
	body := fmt.Sprintf("%s [%s] %s: %s", icon, bot, title, evt.ServiceName)
	if detail != "" {
		body += "\n" + detail
	}
	return body
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

// NotifyDeploySuccess POSTs the success message to Twilio.
func (t *Twilio) NotifyDeploySuccess(ctx context.Context, evt Event) error {
	return t.send(ctx, SuccessMessageBody(evt))
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

func (l *LogFailures) NotifyDeploySuccess(ctx context.Context, evt Event) error {
	err := l.Inner.NotifyDeploySuccess(ctx, evt)
	if err != nil {
		log.Printf("notify: deploy-success alert for %q failed: %v", evt.ServiceName, err)
	}
	return nil
}

// Multi fans every event out to all configured notifiers. A notifier that
// fails does not stop the others; the joined errors are returned for the
// caller to log. An empty Multi behaves like Noop.
type Multi struct {
	Notifiers []Notifier
}

func (m *Multi) NotifyServiceDown(ctx context.Context, evt Event) error {
	return m.each(func(n Notifier) error { return n.NotifyServiceDown(ctx, evt) })
}

func (m *Multi) NotifyServiceRecovered(ctx context.Context, evt Event) error {
	return m.each(func(n Notifier) error { return n.NotifyServiceRecovered(ctx, evt) })
}

func (m *Multi) NotifyDeploySuccess(ctx context.Context, evt Event) error {
	return m.each(func(n Notifier) error { return n.NotifyDeploySuccess(ctx, evt) })
}

func (m *Multi) each(send func(Notifier) error) error {
	var errs []error
	for _, n := range m.Notifiers {
		if err := send(n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
