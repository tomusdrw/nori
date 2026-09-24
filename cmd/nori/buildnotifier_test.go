package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"nori/internal/config"
	"nori/internal/notify"
	"nori/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestBuildNotifier_NoChannelsIsNoop(t *testing.T) {
	if _, ok := buildNotifier(config.Config{}, testStore(t)).(notify.Noop); !ok {
		t.Fatal("no configured channels must yield the no-op notifier")
	}
}

func TestBuildNotifier_WiresStoreBackedRouting(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	cfg := config.Config{
		Twilio:   config.TwilioConfig{AccountSID: "AC", AuthToken: "tok", From: "+1", To: "+2"},
		Telegram: config.TelegramConfig{BotToken: "t", ChatID: "c"},
	}
	lf, ok := buildNotifier(cfg, st).(*notify.LogFailures)
	if !ok {
		t.Fatalf("expected LogFailures wrapper, got %T", lf)
	}
	multi, ok := lf.Inner.(*notify.Multi)
	if !ok {
		t.Fatalf("expected Multi fan-out, got %T", lf.Inner)
	}
	if len(multi.Notifiers) != 2 {
		t.Fatalf("expected two channel routes, got %d", len(multi.Notifiers))
	}
	routes := make(map[string]*notify.Route, 2)
	for _, n := range multi.Notifiers {
		r, ok := n.(*notify.Route)
		if !ok {
			t.Fatalf("expected every channel wrapped in a Route, got %T", n)
		}
		if r.Approve == nil {
			t.Fatalf("route %s must carry an Approve func", r.Channel)
		}
		routes[r.Channel] = r
	}
	for _, ch := range []string{notify.ChannelTwilio, notify.ChannelTelegram} {
		if _, ok := routes[ch]; !ok {
			t.Fatalf("missing route for channel %s", ch)
		}
	}

	// With nothing stored, every event is allowed (historical default).
	if !routes[notify.ChannelTwilio].Approve(notify.KindDown) {
		t.Error("unset settings must allow events")
	}
	if !routes[notify.ChannelTwilio].Approve(notify.KindDeployFailed) {
		t.Error("unset settings must allow deploy failures")
	}

	// A stored routing table gates the matching channel at send time.
	if err := st.SetSetting(ctx, store.SettingNotifyRouting,
		`{"twilio":{"down":false,"recovered":true,"success":true}}`); err != nil {
		t.Fatal(err)
	}
	if routes[notify.ChannelTwilio].Approve(notify.KindDown) {
		t.Error("stored twilio/down=false must block down events")
	}
	if routes[notify.ChannelTwilio].Approve(notify.KindDeployFailed) {
		t.Error("an old twilio/down=false choice must also block deploy failures")
	}
	if !routes[notify.ChannelTwilio].Approve(notify.KindRecovered) {
		t.Error("stored twilio/recovered=true must allow recovery events")
	}
	// Telegram has no entry: absent channels default to enabled.
	if !routes[notify.ChannelTelegram].Approve(notify.KindDown) {
		t.Error("channel absent from the table must default to allowed")
	}
}

func TestBuildNotifier_LegacyModeGatesUntilRoutingSaved(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.SetSetting(ctx, store.SettingNotifyMode, "never"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Telegram: config.TelegramConfig{BotToken: "t", ChatID: "c"},
	}
	lf := buildNotifier(cfg, st).(*notify.LogFailures)
	multi := lf.Inner.(*notify.Multi)
	route := multi.Notifiers[0].(*notify.Route)
	if route.Approve(notify.KindDown) {
		t.Error("legacy never must suppress events until a routing table is saved")
	}
	if err := st.SetSetting(ctx, store.SettingNotifyRouting, `{"telegram":{"down":true,"recovered":true,"success":true}}`); err != nil {
		t.Fatal(err)
	}
	if !route.Approve(notify.KindDown) {
		t.Error("saved routing table must take precedence over the legacy mode")
	}
}

func TestBuildNotifier_TelegramUsesCurrentPublicURL(t *testing.T) {
	var message string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &payload)
		message = payload.Text
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()

	st := testStore(t)
	ctx := context.Background()
	nf := buildNotifier(config.Config{
		Telegram: config.TelegramConfig{BotToken: "token", ChatID: "chat"},
	}, st)
	multi := nf.(*notify.LogFailures).Inner.(*notify.Multi)
	tg := multi.Notifiers[0].(*notify.Route).Inner.(*notify.Telegram)
	tg.BaseURL = api.URL
	tg.Client = api.Client()

	if err := st.SetMCPConfig(ctx, false, "https://nori.example", false); err != nil {
		t.Fatal(err)
	}
	if err := nf.NotifyDeploySuccess(ctx, notify.Event{ServiceName: "billing", DeploymentID: 17}); err != nil {
		t.Fatal(err)
	}
	if want := `href="https://nori.example/deployments/17"`; !strings.Contains(message, want) {
		t.Fatalf("message missing current public URL %q: %q", want, message)
	}

	if err := st.SetMCPConfig(ctx, false, "https://new-nori.example", false); err != nil {
		t.Fatal(err)
	}
	if err := nf.NotifyDeploySuccess(ctx, notify.Event{ServiceName: "billing", DeploymentID: 18}); err != nil {
		t.Fatal(err)
	}
	if want := `href="https://new-nori.example/deployments/18"`; !strings.Contains(message, want) {
		t.Fatalf("message missing updated public URL %q: %q", want, message)
	}
}
