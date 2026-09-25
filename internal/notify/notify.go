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

// messageCopy holds the channel-agnostic content of a notification. The
// plain renderer formats it for SMS and the Telegram renderer adds HTML
// styling and a dashboard link. Every message leads with the service name so
// truncated previews still say which service the event is about.
type messageCopy struct {
	icon      string // event marker, e.g. "✅"
	headline  string // "<service> <verb>", e.g. "billing recovered"
	trigger   string // friendly trigger name, omitted when empty
	digest    string // short image digest
	reason    string // failure reason, empty for recoveries and successes
	linkLabel string // dashboard link text; only Telegram renders links
	instance  string // instance display name, appended to the headline as "@ Instance"
}

// MessageBody formats a short, SMS-friendly body for a service-down event.
// Newlines are kept so most SMS clients render a compact multi-line preview.
func MessageBody(evt Event) string {
	return downCopy(evt).plain()
}

// RecoveredMessageBody formats a short, SMS-friendly body for a
// service-recovered event.
func RecoveredMessageBody(evt Event) string {
	return recoveredCopy(evt).plain()
}

// SuccessMessageBody formats a short body for a successful deployment.
// No reason is included; a successful deploy has nothing to explain.
func SuccessMessageBody(evt Event) string {
	return successCopy(evt).plain()
}

// downCopy assembles the copy for a failed deployment or a monitored outage.
func downCopy(evt Event) messageCopy {
	c := messageCopy{
		icon:      "❌",
		headline:  evt.ServiceName + " deploy failed",
		digest:    evt.Digest,
		reason:    evt.Reason,
		linkLabel: "View deployment",
		instance:  instanceName(evt),
	}
	if evt.Trigger == store.TriggerMonitor {
		c.icon, c.headline, c.linkLabel = "🚨", evt.ServiceName+" is down", "View service"
	} else if evt.Trigger != "" {
		c.trigger = friendlyTrigger(evt.Trigger)
	}
	return c
}

// recoveredCopy assembles the copy for a service-recovered event. The digest
// is the only detail; a recovery has nothing to explain either.
func recoveredCopy(evt Event) messageCopy {
	return messageCopy{
		icon:      "✅",
		headline:  evt.ServiceName + " recovered",
		digest:    evt.Digest,
		linkLabel: "View service",
		instance:  instanceName(evt),
	}
}

// successCopy assembles the copy for a successful deployment.
func successCopy(evt Event) messageCopy {
	c := messageCopy{
		icon:      "🚀",
		headline:  evt.ServiceName + " deployed",
		digest:    evt.Digest,
		linkLabel: "View deployment",
		instance:  instanceName(evt),
	}
	if evt.Trigger != "" {
		c.trigger = friendlyTrigger(evt.Trigger)
	}
	return c
}

// instanceName returns the instance display name for the footer, defaulting
// to "Nori" for unconfigured instances.
func instanceName(evt Event) string {
	if bot := strings.TrimSpace(evt.BotName); bot != "" {
		return bot
	}
	return "Nori"
}

// friendlyTrigger maps stored trigger values to readable names.
func friendlyTrigger(trigger string) string {
	switch trigger {
	case store.TriggerManual:
		return "Manual"
	case store.TriggerAuto:
		return "Automatic"
	case store.TriggerScheduled:
		return "Scheduled"
	case store.TriggerMonitor:
		return "Health monitor"
	default:
		return trigger
	}
}

// detail joins the trigger, digest, and reason into one compact metadata
// line, e.g. "Manual · sha256:abc12345 — exit status 1". The text and code
// functions style individual fragments; plain rendering passes them through.
func (c messageCopy) detail(text, code func(string) string) string {
	parts := make([]string, 0, 2)
	if c.trigger != "" {
		parts = append(parts, text(c.trigger))
	}
	if c.digest != "" {
		parts = append(parts, code(c.digest))
	}
	line := strings.Join(parts, " · ")
	if c.reason != "" {
		if line != "" {
			line += " — "
		}
		line += text(c.reason)
	}
	return line
}

// plain renders the copy as plain text for SMS. Links are omitted: Twilio
// has no dashboard URL to point at.
func (c messageCopy) plain() string {
	lines := []string{c.icon + " " + c.headline + " @ " + c.instance}
	if detail := c.detail(plainText, plainText); detail != "" {
		lines = append(lines, detail)
	}
	return strings.Join(lines, "\n")
}

func plainText(s string) string { return s }

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
