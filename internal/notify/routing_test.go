package notify

import (
	"strings"
	"testing"
)

func TestDefaultRouting_EnablesEveryKindForEveryChannel(t *testing.T) {
	r := DefaultRouting()
	for _, ch := range Channels {
		for _, kind := range Kinds {
			if !r.Allowed(ch, kind) {
				t.Errorf("default routing must allow %s/%s", ch, kind)
			}
		}
	}
}

func TestRouting_Allowed(t *testing.T) {
	r := Routing{
		ChannelTwilio: {KindDown: true, KindSuccess: false},
	}
	if !r.Allowed(ChannelTwilio, KindDown) {
		t.Error("explicitly enabled kind must be allowed")
	}
	if r.Allowed(ChannelTwilio, KindSuccess) {
		t.Error("explicitly disabled kind must not be allowed")
	}
	if r.Allowed(ChannelTwilio, KindRecovered) {
		t.Error("kind missing from a present channel defaults to disabled")
	}
	// A channel absent from the table (e.g. configured via env after the
	// table was saved) starts enabled, matching the historical behavior.
	for _, kind := range Kinds {
		if !r.Allowed(ChannelTelegram, kind) {
			t.Errorf("absent channel must default to allowed for %s", kind)
		}
	}
}

func TestParseRouting_DecodesStoredJSON(t *testing.T) {
	r, err := ParseRouting(`{"twilio":{"down":true,"recovered":false,"success":false},"telegram":{"down":false,"recovered":true,"success":true}}`)
	if err != nil {
		t.Fatalf("ParseRouting: %v", err)
	}
	if !r.Allowed(ChannelTwilio, KindDown) {
		t.Error("twilio/down must be enabled")
	}
	if r.Allowed(ChannelTwilio, KindRecovered) || r.Allowed(ChannelTwilio, KindSuccess) {
		t.Error("twilio/recovered and twilio/success must be disabled")
	}
	if r.Allowed(ChannelTelegram, KindDown) {
		t.Error("telegram/down must be disabled")
	}
	if !r.Allowed(ChannelTelegram, KindRecovered) || !r.Allowed(ChannelTelegram, KindSuccess) {
		t.Error("telegram/recovered and telegram/success must be enabled")
	}
}

func TestParseRouting_DropsUnknownKeys(t *testing.T) {
	r, err := ParseRouting(`{"pagerduty":{"down":true},"twilio":{"down":true,"pagerduty":true}}`)
	if err != nil {
		t.Fatalf("ParseRouting: %v", err)
	}
	if r.Allowed(ChannelTwilio, KindDown) != true {
		t.Error("known channel/kind must survive")
	}
	if _, ok := r[ChannelTelegram]; ok {
		t.Error("channel absent from JSON must stay absent (so it defaults to allowed)")
	}
}

func TestParseRouting_RejectsGarbage(t *testing.T) {
	if _, err := ParseRouting("not json"); err == nil {
		t.Fatal("ParseRouting must reject unparseable JSON")
	}
}

func TestRoutingFromLegacy(t *testing.T) {
	cases := []struct {
		mode string
		want bool // whether any kind is allowed
	}{
		{"always", true},
		{"", true},
		{"unrecognized", true},
		{"auto-only", true}, // manual/auto distinction is gone; approximate as all-on
		{"never", false},
	}
	for _, tc := range cases {
		r := RoutingFromLegacy(tc.mode)
		got := r.Allowed(ChannelTwilio, KindDown)
		if got != tc.want {
			t.Errorf("RoutingFromLegacy(%q).Allowed(twilio,down) = %v, want %v", tc.mode, got, tc.want)
		}
		for _, ch := range Channels {
			for _, kind := range Kinds {
				if r.Allowed(ch, kind) != tc.want {
					t.Errorf("RoutingFromLegacy(%q) must set %s/%s to %v uniformly", tc.mode, ch, kind, tc.want)
				}
			}
		}
	}
}

func TestEffectiveRouting_StoredTableWins(t *testing.T) {
	r := EffectiveRouting(`{"twilio":{"down":false}}`, "never")
	if !r.Allowed(ChannelTelegram, KindDown) {
		t.Error("stored table must win over legacy mode; telegram absent from table defaults to allowed")
	}
	if r.Allowed(ChannelTwilio, KindDown) {
		t.Error("stored table must win over legacy mode; twilio/down stored as off")
	}
}

func TestEffectiveRouting_FallsBackToLegacyMode(t *testing.T) {
	r := EffectiveRouting("", "never")
	if r.Allowed(ChannelTwilio, KindDown) {
		t.Error("empty routing with legacy never must disable events")
	}
	r = EffectiveRouting("", "always")
	if !r.Allowed(ChannelTwilio, KindDown) {
		t.Error("empty routing with legacy always must enable events")
	}
}

func TestEffectiveRouting_CorruptTableFallsBackToDefaults(t *testing.T) {
	r := EffectiveRouting("{oops", "")
	for _, ch := range Channels {
		for _, kind := range Kinds {
			if !r.Allowed(ch, kind) {
				t.Errorf("corrupt table must fall back to all-enabled; %s/%s disabled", ch, kind)
			}
		}
	}
}

func TestMarshalRoutingRoundTrip(t *testing.T) {
	r := DefaultRouting()
	r[ChannelTwilio] = map[EventKind]bool{KindDown: true, KindRecovered: false, KindSuccess: false}
	b, err := MarshalRouting(r)
	if err != nil {
		t.Fatalf("marshalRouting: %v", err)
	}
	if !strings.Contains(b, `"down":true`) {
		t.Errorf("expected down:true in JSON, got %s", b)
	}
	parsed, err := ParseRouting(b)
	if err != nil {
		t.Fatalf("ParseRouting: %v", err)
	}
	if !parsed.Allowed(ChannelTwilio, KindDown) || parsed.Allowed(ChannelTwilio, KindRecovered) {
		t.Errorf("round-trip changed the table: %+v", parsed)
	}
}
