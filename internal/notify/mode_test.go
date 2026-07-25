package notify

import "testing"

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeAlways, false},
		{"   ", ModeAlways, false},
		{"always", ModeAlways, false},
		{"ALWAYS", ModeAlways, false},
		{"  Auto-Only  ", ModeAutoOnly, false},
		{"never", ModeNever, false},
		{"disabled", "", true},
		{"off", "", true},
		{"auto", "", true}, // not a valid mode; "auto" is a trigger, not a mode
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := NormalizeMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeMode(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeMode(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShouldSend(t *testing.T) {
	cases := []struct {
		mode    Mode
		trigger string
		want    bool
	}{
		{ModeAlways, "manual", true},
		{ModeAlways, "auto", true},
		{ModeAlways, "scheduled", true},
		{ModeAlways, "", true},
		{ModeAlways, "monitor", true},

		{ModeAutoOnly, "manual", false},
		{ModeAutoOnly, "auto", true},
		{ModeAutoOnly, "scheduled", true},
		{ModeAutoOnly, "monitor", true},

		{ModeNever, "manual", false},
		{ModeNever, "auto", false},
		{ModeNever, "scheduled", false},
		{ModeNever, "monitor", false},

		// Unknown mode falls back to "always" semantics.
		{"", "manual", true},
		{"bogus", "manual", true},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode)+"_"+tc.trigger, func(t *testing.T) {
			if got := ShouldSend(tc.mode, tc.trigger); got != tc.want {
				t.Errorf("ShouldSend(%q, %q) = %v, want %v", tc.mode, tc.trigger, got, tc.want)
			}
		})
	}
}
