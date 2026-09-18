package envfile

import "testing"

func TestRedactor(t *testing.T) {
	var r Redactor
	if err := r.Add("A=abc\nB=bcd\nC='line1\nline2'\nEMPTY=\nCOPY=${A}"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text       string
		start, end int
		want       string
	}{
		{"before abcd after", 0, 17, "before [REDACTED] after"},
		{"xabcx", 0, 3, "x[REDACTED]"},
		{"xabcx", 3, 5, "[REDACTED]x"},
		{"line1\nline2", 0, 11, "[REDACTED]"},
		{"line2\n", 0, 6, "[REDACTED]\n"},
		{"", 0, 0, ""},
	} {
		if got := r.Window(tc.text, tc.start, tc.end); got != tc.want {
			t.Fatalf("redaction: got %q, want %q", got, tc.want)
		}
	}
}
