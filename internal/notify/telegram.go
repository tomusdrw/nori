package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
	return t.send(ctx, MessageBody(evt))
}

func (t *Telegram) NotifyServiceRecovered(ctx context.Context, evt Event) error {
	return t.send(ctx, RecoveredMessageBody(evt))
}

func (t *Telegram) NotifyDeploySuccess(ctx context.Context, evt Event) error {
	return t.send(ctx, SuccessMessageBody(evt))
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
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: t.ChatID, Text: body})
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
