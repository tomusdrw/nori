package notify

import (
	"context"
	"errors"
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
	if err := (Noop{}).NotifyDeploySuccess(context.Background(), Event{ServiceName: "app"}); err != nil {
		t.Fatalf("noop must never error: %v", err)
	}
}

func TestSuccessMessageBody_HasNoReason(t *testing.T) {
	got := SuccessMessageBody(Event{
		BotName:     "prod",
		ServiceName: "billing",
		Trigger:     "scheduled",
		Digest:      "sha256:abc12345",
		Reason:      "must be ignored",
	})
	want := "🚀 billing deployed @ prod\nScheduled · sha256:abc12345"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestSuccessMessageBody_DefaultsBotName(t *testing.T) {
	got := SuccessMessageBody(Event{ServiceName: "x"})
	if want := "🚀 x deployed @ Nori"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestSMSMessageBodies_LeadWithServiceName(t *testing.T) {
	evt := Event{
		BotName:     "Production",
		ServiceName: "billing",
		Trigger:     "auto",
		Digest:      "sha256:abc12345",
		Reason:      "exit status 1",
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "failed deployment",
			body: MessageBody(evt),
			want: "❌ billing deploy failed @ Production\nAutomatic · sha256:abc12345 — exit status 1",
		},
		{
			name: "monitored outage",
			body: MessageBody(Event{
				BotName:     evt.BotName,
				ServiceName: evt.ServiceName,
				Trigger:     "monitor",
				Digest:      evt.Digest,
				Reason:      "no containers",
			}),
			want: "🚨 billing is down @ Production\nsha256:abc12345 — no containers",
		},
		{
			name: "recovery",
			body: RecoveredMessageBody(evt),
			want: "✅ billing recovered @ Production\nsha256:abc12345",
		},
		{
			name: "successful deployment",
			body: SuccessMessageBody(evt),
			want: "🚀 billing deployed @ Production\nAutomatic · sha256:abc12345",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.body != tc.want {
				t.Errorf("body = %q, want %q", tc.body, tc.want)
			}
		})
	}
}

// recordingNotifier records the calls it receives and optionally fails.
type recordingNotifier struct {
	name  string
	err   error
	calls []string
}

func (r *recordingNotifier) NotifyServiceDown(_ context.Context, _ Event) error {
	r.calls = append(r.calls, "down")
	return r.err
}

func (r *recordingNotifier) NotifyServiceRecovered(_ context.Context, _ Event) error {
	r.calls = append(r.calls, "recovered")
	return r.err
}

func (r *recordingNotifier) NotifyDeploySuccess(_ context.Context, _ Event) error {
	r.calls = append(r.calls, "success")
	return r.err
}

func TestMulti_DeliversToAllNotifiers(t *testing.T) {
	a := &recordingNotifier{name: "a"}
	b := &recordingNotifier{name: "b"}
	m := &Multi{Notifiers: []Notifier{a, b}}

	if err := m.NotifyServiceDown(context.Background(), Event{}); err != nil {
		t.Fatalf("NotifyServiceDown: %v", err)
	}
	if err := m.NotifyServiceRecovered(context.Background(), Event{}); err != nil {
		t.Fatalf("NotifyServiceRecovered: %v", err)
	}
	if err := m.NotifyDeploySuccess(context.Background(), Event{}); err != nil {
		t.Fatalf("NotifyDeploySuccess: %v", err)
	}
	for _, n := range []*recordingNotifier{a, b} {
		if want := []string{"down", "recovered", "success"}; strings.Join(n.calls, ",") != strings.Join(want, ",") {
			t.Errorf("notifier %s calls = %v, want %v", n.name, n.calls, want)
		}
	}
}

func TestMulti_ContinuesAfterFailure(t *testing.T) {
	failing := &recordingNotifier{name: "failing", err: errors.New("channel down")}
	healthy := &recordingNotifier{name: "healthy"}
	m := &Multi{Notifiers: []Notifier{failing, healthy}}

	err := m.NotifyServiceDown(context.Background(), Event{ServiceName: "app"})
	if err == nil {
		t.Fatal("expected the joined error from the failing notifier")
	}
	if !strings.Contains(err.Error(), "channel down") {
		t.Errorf("error = %v, want it to mention the failure", err)
	}
	if len(healthy.calls) != 1 {
		t.Fatalf("healthy notifier calls = %v, want it to still receive the event", healthy.calls)
	}
}

func TestMulti_EmptyBehavesLikeNoop(t *testing.T) {
	m := &Multi{}
	if err := m.NotifyServiceDown(context.Background(), Event{}); err != nil {
		t.Fatalf("empty Multi must not error: %v", err)
	}
}

func TestMessageBody_IncludesFieldsAndDefaultBot(t *testing.T) {
	got := MessageBody(Event{
		ServiceName: "billing-api",
		Trigger:     "auto",
		Digest:      "sha256:abc12345",
		Reason:      "exit status 1",
	})
	if !strings.Contains(got, "@ Nori") {
		t.Errorf("body should default to Nori: %q", got)
	}
	for _, want := range []string{"billing-api", "Automatic", "sha256:abc12345", "exit status 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q: %q", want, got)
		}
	}
}

func TestMessageBody_UsesCustomBotName(t *testing.T) {
	got := MessageBody(Event{BotName: "prod", ServiceName: "x"})
	if !strings.HasPrefix(got, "❌ x deploy failed @ prod") {
		t.Errorf("body should lead with the service and instance: %q", got)
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

func TestLogFailures_NotifyServiceRecovered_SwallowsButLogs(t *testing.T) {
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
	if err := wrapped.NotifyServiceRecovered(context.Background(), Event{ServiceName: "app"}); err != nil {
		t.Errorf("LogFailures must not propagate: %v", err)
	}
}

func TestLogFailures_NotifyDeploySuccess_SwallowsButLogs(t *testing.T) {
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
	if err := wrapped.NotifyDeploySuccess(context.Background(), Event{ServiceName: "app"}); err != nil {
		t.Errorf("LogFailures must not propagate: %v", err)
	}
}

func TestTwilio_NotifyServiceRecovered_PostsExpectedRequest(t *testing.T) {
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
	// Recovered event payload
	err := tw.NotifyServiceRecovered(context.Background(), Event{
		BotName:     "prod-nori",
		ServiceName: "billing",
		Trigger:     "monitor",
		Digest:      "sha256:deadbeef",
		Reason:      "service recovered",
	})
	if err != nil {
		t.Fatalf("NotifyServiceRecovered: %v", err)
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
	if !strings.Contains(vals.Get("Body"), "billing") || !strings.Contains(vals.Get("Body"), "prod-nori") || !strings.Contains(vals.Get("Body"), "billing recovered") {
		t.Errorf("Body = %q", vals.Get("Body"))
	}
}
