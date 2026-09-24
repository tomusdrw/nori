package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"nori/internal/store"
)

// ErrMissingTelegramConfig is returned when the notifier was constructed
// without the fields Telegram requires. Callers should use
// config.TelegramConfig.Enabled to decide whether to build a Telegram
// notifier at all.
var ErrMissingTelegramConfig = errors.New("telegram notifier is missing required configuration")

// Telegram posts messages via the Telegram Bot API sendMessage method.
// Construct one with NewTelegram; a nil client uses http.DefaultClient.
type Telegram struct {
	BaseURL  string // defaults to "https://api.telegram.org"
	BotToken string
	ChatID   string
	Client   *http.Client
	// PublicURL resolves the current externally reachable Nori origin. When it
	// is nil or returns empty, messages omit dashboard links.
	PublicURL func(context.Context) string
}

// NewTelegram returns a Telegram notifier. Callers should pass a populated
// config; validation is the caller's responsibility (see
// config.TelegramConfig.Enabled).
func NewTelegram(botToken, chatID string) *Telegram {
	return &Telegram{
		BaseURL:  "https://api.telegram.org",
		BotToken: botToken,
		ChatID:   chatID,
		Client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *Telegram) NotifyServiceDown(ctx context.Context, evt Event) error {
	icon, title := "❌", "Deployment failed"
	linkLabel := "View deployment"
	publicURL := t.publicURL(ctx)
	link := deploymentLink(publicURL, evt.DeploymentID)
	if evt.Trigger == store.TriggerMonitor {
		icon, title = "🚨", "Service is down"
		linkLabel = "View service"
		link = serviceLink(publicURL, evt.ServiceName)
	}
	return t.send(ctx, telegramMessage(icon, title, evt, true, linkLabel, link))
}

func (t *Telegram) NotifyServiceRecovered(ctx context.Context, evt Event) error {
	link := serviceLink(t.publicURL(ctx), evt.ServiceName)
	return t.send(ctx, telegramMessage("✅", "Service recovered", evt, false, "View service", link))
}

func (t *Telegram) NotifyDeploySuccess(ctx context.Context, evt Event) error {
	link := deploymentLink(t.publicURL(ctx), evt.DeploymentID)
	return t.send(ctx, telegramMessage("🚀", "Deployment succeeded", evt, false, "View deployment", link))
}

func telegramMessage(icon, title string, evt Event, includeReason bool, linkLabel, link string) string {
	bot := strings.TrimSpace(evt.BotName)
	if bot == "" {
		bot = "Nori"
	}
	lines := []string{
		icon + " <b>" + title + "</b>",
		"",
		"<b>Service:</b> " + html.EscapeString(evt.ServiceName),
		"<b>Instance:</b> " + html.EscapeString(bot),
	}
	if evt.Trigger != "" {
		lines = append(lines, "<b>Trigger:</b> "+html.EscapeString(friendlyTrigger(evt.Trigger)))
	}
	if evt.Digest != "" {
		lines = append(lines, "<b>Image:</b> <code>"+html.EscapeString(evt.Digest)+"</code>")
	}
	if includeReason && evt.Reason != "" {
		lines = append(lines, "<b>Reason:</b> "+html.EscapeString(evt.Reason))
	}
	if link != "" {
		lines = append(lines, "", "🔗 <a href=\""+html.EscapeString(link)+"\">"+linkLabel+"</a>")
	}
	return strings.Join(lines, "\n")
}

func (t *Telegram) publicURL(ctx context.Context) string {
	if t == nil || t.PublicURL == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(t.PublicURL(ctx)), "/")
}

func deploymentLink(publicURL string, deploymentID int64) string {
	if publicURL == "" || deploymentID <= 0 {
		return ""
	}
	return publicURL + "/deployments/" + strconv.FormatInt(deploymentID, 10)
}

func serviceLink(publicURL, serviceName string) string {
	if publicURL == "" || serviceName == "" {
		return ""
	}
	return publicURL + "/services/" + url.PathEscape(serviceName)
}

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

// send POSTs a preformatted message body to the Bot API. Errors never
// include the bot token: it is part of the endpoint URL and would otherwise
// leak through wrapped transport errors.
func (t *Telegram) send(ctx context.Context, body string) error {
	if t == nil || t.BotToken == "" || t.ChatID == "" {
		return ErrMissingTelegramConfig
	}
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(base, "/"), t.BotToken)

	payload, err := json.Marshal(struct {
		ChatID    string `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}{ChatID: t.ChatID, Text: body, ParseMode: "HTML"})
	if err != nil {
		return fmt.Errorf("telegram: encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram: build request: %s", t.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")

	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send: %s", t.redact(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Telegram replies with {"ok":false,"error_code":...,"description":"..."}.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		var tgErr struct {
			Description string `json:"description"`
			ErrorCode   int    `json:"error_code"`
		}
		_ = json.Unmarshal(body, &tgErr)
		detail := strings.TrimSpace(tgErr.Description)
		if detail == "" {
			detail = strings.TrimSpace(string(body))
		}
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("telegram: %s: %s (code %d)",
			t.redact(resp.Status), t.redact(detail), tgErr.ErrorCode)
	}
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// redact replaces every occurrence of the bot token or chat ID with a
// placeholder so error strings are safe to log.
func (t *Telegram) redact(s string) string {
	for _, secret := range []string{t.BotToken, t.ChatID} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}
