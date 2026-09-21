package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTelegram_RequiresAllFields(t *testing.T) {
	cases := []struct {
		name string
		tg   Telegram
	}{
		{"missing token", Telegram{ChatID: "1"}},
		{"missing chat id", Telegram{BotToken: "tok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tg.NotifyServiceDown(context.Background(), Event{ServiceName: "app"})
			if err != ErrMissingTelegramConfig {
				t.Fatalf("err = %v, want ErrMissingTelegramConfig", err)
			}
		})
	}
}

func TestTelegram_NilReceiverIsMissingConfig(t *testing.T) {
	var tg *Telegram
	if err := tg.NotifyServiceDown(context.Background(), Event{}); err != ErrMissingTelegramConfig {
		t.Fatalf("err = %v, want ErrMissingTelegramConfig", err)
	}
}

func TestTelegram_PostsExpectedRequest(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer srv.Close()

	tg := &Telegram{
		BaseURL:  srv.URL,
		BotToken: "secret-token",
		ChatID:   "-1001234567890",
		Client:   srv.Client(),
	}
	err := tg.NotifyServiceDown(context.Background(), Event{
		BotName:     "prod-nori",
		ServiceName: "billing",
		Trigger:     "auto",
		Digest:      "sha256:deadbeef",
		Reason:      "container exited",
	})
	if err != nil {
		t.Fatalf("NotifyServiceDown: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "/botsecret-token/sendMessage"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	var payload struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v (%s)", err, gotBody)
	}
	if payload.ChatID != "-1001234567890" {
		t.Errorf("chat_id = %q", payload.ChatID)
	}
	for _, want := range []string{"prod-nori", "billing", "trigger=auto", "sha256:deadbeef", "container exited"} {
		if !strings.Contains(payload.Text, want) {
			t.Errorf("text missing %q: %q", want, payload.Text)
		}
	}
	for _, secret := range []string{"secret-token", "-1001234567890"} {
		if strings.Contains(payload.Text, secret) {
			t.Errorf("text must not contain %q: %q", secret, payload.Text)
		}
	}
}

func TestTelegram_Non2xxSurfacesDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()

	tg := &Telegram{
		BaseURL:  srv.URL,
		BotToken: "tok",
		ChatID:   "1",
		Client:   srv.Client(),
	}
	err := tg.NotifyServiceDown(context.Background(), Event{ServiceName: "app"})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	for _, want := range []string{"400", "chat not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestTelegram_TransportErrorRedactsToken(t *testing.T) {
	// Port 1 is never listening, so the request fails in the transport layer
	// where *url.Error embeds the full endpoint URL, token included.
	tg := &Telegram{
		BaseURL:  "http://127.0.0.1:1",
		BotToken: "super-secret-token",
		ChatID:   "555000111222",
		Client:   &http.Client{Timeout: 2 * time.Second},
	}
	err := tg.NotifyServiceDown(context.Background(), Event{ServiceName: "app"})
	if err == nil {
		t.Fatal("expected transport error")
	}
	for _, secret := range []string{"super-secret-token", "555000111222"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %q", secret, err.Error())
		}
	}
}

func TestTelegram_BoundedTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{
		BaseURL:  srv.URL,
		BotToken: "tok",
		ChatID:   "1",
		Client:   &http.Client{Timeout: 50 * time.Millisecond},
	}
	start := time.Now()
	err := tg.NotifyServiceDown(context.Background(), Event{ServiceName: "app"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("send took %v, client timeout not applied", elapsed)
	}
}

func TestTelegram_AllEventsUseFormatting(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &payload)
		bodies = append(bodies, payload.Text)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{
		BaseURL:  srv.URL,
		BotToken: "tok",
		ChatID:   "1",
		Client:   srv.Client(),
	}
	evt := Event{BotName: "n", ServiceName: "app", Trigger: "manual", Digest: "sha256:x", Reason: "boom"}
	bodies = nil
	if err := tg.NotifyServiceDown(context.Background(), evt); err != nil {
		t.Fatalf("NotifyServiceDown: %v", err)
	}
	if bodies[0] != MessageBody(evt) {
		t.Errorf("down body = %q, want %q", bodies[0], MessageBody(evt))
	}
	bodies = nil
	if err := tg.NotifyServiceRecovered(context.Background(), evt); err != nil {
		t.Fatalf("NotifyServiceRecovered: %v", err)
	}
	if bodies[0] != RecoveredMessageBody(evt) {
		t.Errorf("recovered body = %q, want %q", bodies[0], RecoveredMessageBody(evt))
	}
	bodies = nil
	if err := tg.NotifyDeploySuccess(context.Background(), evt); err != nil {
		t.Fatalf("NotifyDeploySuccess: %v", err)
	}
	if bodies[0] != SuccessMessageBody(evt) {
		t.Errorf("success body = %q, want %q", bodies[0], SuccessMessageBody(evt))
	}
}
