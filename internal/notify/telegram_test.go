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
	for _, want := range []string{"prod-nori", "billing", "❌ <b>billing deploy failed</b> @ prod-nori", "Automatic · <code>sha256:deadbeef</code> — container exited"} {
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

func TestTelegram_AllEventsLeadWithServiceName(t *testing.T) {
	type sentMessage struct {
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	var messages []sentMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload sentMessage
		_ = json.Unmarshal(raw, &payload)
		messages = append(messages, payload)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{
		BaseURL:  srv.URL,
		BotToken: "tok",
		ChatID:   "1",
		Client:   srv.Client(),
	}

	evt := Event{BotName: "Production", ServiceName: "billing", Trigger: "manual", Digest: "sha256:x", Reason: "boom"}
	calls := []struct {
		name    string
		send    func() error
		want    []string
		mustNot []string
	}{
		{
			name: "failed deployment",
			send: func() error { return tg.NotifyServiceDown(context.Background(), evt) },
			want: []string{"❌ <b>billing deploy failed</b> @ Production", "Manual · <code>sha256:x</code> — boom"},
		},
		{
			name: "monitored outage",
			send: func() error {
				outage := evt
				outage.Trigger = "monitor"
				outage.Reason = "no containers"
				return tg.NotifyServiceDown(context.Background(), outage)
			},
			want: []string{"🚨 <b>billing is down</b> @ Production", "<code>sha256:x</code> — no containers"},
		},
		{
			name: "recovery",
			send: func() error {
				recovered := evt
				recovered.Trigger = "monitor"
				return tg.NotifyServiceRecovered(context.Background(), recovered)
			},
			want:    []string{"✅ <b>billing recovered</b> @ Production", "<code>sha256:x</code>"},
			mustNot: []string{"boom"},
		},
		{
			name:    "successful deployment",
			send:    func() error { return tg.NotifyDeploySuccess(context.Background(), evt) },
			want:    []string{"🚀 <b>billing deployed</b> @ Production", "Manual · <code>sha256:x</code>"},
			mustNot: []string{"boom"},
		},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			messages = nil
			if err := tc.send(); err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 {
				t.Fatalf("sent %d messages, want 1", len(messages))
			}
			if messages[0].ParseMode != "HTML" {
				t.Errorf("parse_mode = %q, want HTML", messages[0].ParseMode)
			}
			for _, want := range tc.want {
				if !strings.Contains(messages[0].Text, want) {
					t.Errorf("text missing %q: %q", want, messages[0].Text)
				}
			}
			for _, banned := range tc.mustNot {
				if strings.Contains(messages[0].Text, banned) {
					t.Errorf("text must not contain %q: %q", banned, messages[0].Text)
				}
			}
		})
	}
}

func TestTelegram_AddsContextualDashboardLinks(t *testing.T) {
	var messages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &payload)
		messages = append(messages, payload.Text)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{
		BaseURL:  srv.URL,
		BotToken: "tok",
		ChatID:   "1",
		Client:   srv.Client(),
		PublicURL: func(context.Context) string {
			return "https://nori.example"
		},
	}
	deployment := Event{ServiceName: "billing", Trigger: "manual", DeploymentID: 42}
	outage := Event{ServiceName: "billing", Trigger: "monitor"}
	cases := []struct {
		name string
		send func() error
		want string
	}{
		{"failed deployment", func() error { return tg.NotifyServiceDown(context.Background(), deployment) }, `🔗 <a href="https://nori.example/deployments/42">View deployment</a>`},
		{"successful deployment", func() error { return tg.NotifyDeploySuccess(context.Background(), deployment) }, `🔗 <a href="https://nori.example/deployments/42">View deployment</a>`},
		{"monitored outage", func() error { return tg.NotifyServiceDown(context.Background(), outage) }, `🔗 <a href="https://nori.example/services/billing">View service</a>`},
		{"recovery", func() error { return tg.NotifyServiceRecovered(context.Background(), outage) }, `🔗 <a href="https://nori.example/services/billing">View service</a>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			messages = nil
			if err := tc.send(); err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 || !strings.Contains(messages[0], tc.want) {
				t.Errorf("message missing link %q: %q", tc.want, messages)
			}
		})
	}
}

func TestTelegram_OmitsDashboardLinkWhenUnavailable(t *testing.T) {
	var message string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &payload)
		message = payload.Text
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{BaseURL: srv.URL, BotToken: "tok", ChatID: "1", Client: srv.Client()}
	if err := tg.NotifyDeploySuccess(context.Background(), Event{ServiceName: "billing", DeploymentID: 42}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(message, "<a href=") || strings.Contains(message, "View deployment") {
		t.Errorf("message must omit an unavailable link: %q", message)
	}
}

func TestTelegram_EscapesDynamicHTML(t *testing.T) {
	var message string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &payload)
		message = payload.Text
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{BaseURL: srv.URL, BotToken: "tok", ChatID: "1", Client: srv.Client()}
	evt := Event{
		BotName:     "Prod <west>",
		ServiceName: "billing & jobs",
		Trigger:     "manual",
		Digest:      "sha256:<bad>",
		Reason:      "<script> & failed",
	}
	if err := tg.NotifyServiceDown(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Prod &lt;west&gt;", "billing &amp; jobs", "sha256:&lt;bad&gt;", "&lt;script&gt; &amp; failed"} {
		if !strings.Contains(message, want) {
			t.Errorf("escaped message missing %q: %q", want, message)
		}
	}
}
