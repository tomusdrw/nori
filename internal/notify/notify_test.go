package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNoop_NeverErrors(t *testing.T) {
	if err := (Noop{}).NotifyServiceDown(context.Background(), Event{ServiceName: "app"}); err != nil {
		t.Fatalf("noop must never error: %v", err)
	}
}

func TestMessageBody_IncludesFieldsAndDefaultBot(t *testing.T) {
	got := MessageBody(Event{
		ServiceName: "billing-api",
		Trigger:     "auto",
		Digest:      "sha256:abc12345",
		Reason:      "exit status 1",
	})
	if !strings.Contains(got, "[Nori]") {
		t.Errorf("body missing default bot name: %q", got)
	}
	for _, want := range []string{"billing-api", "trigger=auto", "sha256:abc12345", "exit status 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q: %q", want, got)
		}
	}
}

func TestMessageBody_UsesCustomBotName(t *testing.T) {
	got := MessageBody(Event{BotName: "prod", ServiceName: "x"})
	if !strings.HasPrefix(got, "[prod]") {
		t.Errorf("body should use custom bot name: %q", got)
	}
}

func TestTwilio_RequiresAllFields(t *testing.T) {
	cases := []struct {
		name string
		t    Twilio
	}{
		{"missing sid", Twilio{AuthToken: "x", From: "+1", To: "+2"}},
		{"missing token", Twilio{AccountSID: "x", From: "+1", To: "+2"}},
		{"missing from", Twilio{AccountSID: "x", AuthToken: "y", To: "+2"}},
		{"missing to", Twilio{AccountSID: "x", AuthToken: "y", From: "+1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.t.NotifyServiceDown(context.Background(), Event{ServiceName: "app"})
			if err != ErrMissingConfig {
				t.Fatalf("err = %v, want ErrMissingConfig", err)
			}
		})
	}
}

func TestTwilio_NilReceiverIsMissingConfig(t *testing.T) {
	var tw *Twilio
	if err := tw.NotifyServiceDown(context.Background(), Event{}); err != ErrMissingConfig {
		t.Fatalf("err = %v, want ErrMissingConfig", err)
	}
}

func TestTwilio_PostsExpectedRequest(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotUser   string
		gotBody   string
		gotCT     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		u, _, _ := r.BasicAuth()
		gotUser = u
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM123"}`))
	}))
	defer srv.Close()

	tw := &Twilio{
		BaseURL:    srv.URL,
		AccountSID: "AC123",
		AuthToken:  "secret-token",
		From:       "+15551234567",
		To:         "+15559876543",
		Client:     srv.Client(),
	}
	err := tw.NotifyServiceDown(context.Background(), Event{
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
	if want := "/2010-04-01/Accounts/AC123/Messages.json"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotUser != "AC123" {
		t.Errorf("basic auth user = %q, want AC123", gotUser)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", gotCT)
	}
	vals, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if vals.Get("From") != "+15551234567" {
		t.Errorf("From = %q", vals.Get("From"))
	}
	if vals.Get("To") != "+15559876543" {
		t.Errorf("To = %q", vals.Get("To"))
	}
	if !strings.Contains(vals.Get("Body"), "billing") ||
		!strings.Contains(vals.Get("Body"), "prod-nori") ||
		!strings.Contains(vals.Get("Body"), "container exited") {
		t.Errorf("Body = %q", vals.Get("Body"))
	}
}

func TestTwilio_SurfaceAPIErrorDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":21211,"message":"The 'To' number is not a valid phone number"}`))
	}))
	defer srv.Close()

	tw := &Twilio{
		BaseURL:    srv.URL,
		AccountSID: "AC123",
		AuthToken:  "tok",
		From:       "+1",
		To:         "+1",
		Client:     srv.Client(),
	}
	err := tw.NotifyServiceDown(context.Background(), Event{ServiceName: "app"})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	for _, want := range []string{"400", "21211", "not a valid phone number"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestLogFailures_SwallowsButLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tw := &Twilio{
		BaseURL:    srv.URL,
		AccountSID: "AC",
		AuthToken:  "t",
		From:       "+1",
		To:         "+2",
		Client:     srv.Client(),
	}
	wrapped := &LogFailures{Inner: tw}
	if err := wrapped.NotifyServiceDown(context.Background(), Event{ServiceName: "app"}); err != nil {
		t.Errorf("LogFailures must not propagate: %v", err)
	}
}
