package envfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestTemplateNeverReturnsValuesOrComments(t *testing.T) {
	raw := "# secret comment\nexport TOKEN='private\nmultiline' # private comment\nPORT=08080\nEMPTY=\nCOPY=${TOKEN}\n"
	got, err := Template(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != "COPY=\"[REDACTED]\"\nEMPTY=\"[REDACTED]\"\nPORT=\"[REDACTED]\"\nTOKEN=\"[REDACTED]\"\n" {
		t.Fatalf("unexpected template: %q", got)
	}
	resolved, err := ResolveTemplate(raw, got)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := Parse(raw)
	after, _ := Parse(resolved)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("template round trip changed values")
	}
}

func TestResolveTemplatePreservesOnlySameKey(t *testing.T) {
	got, err := ResolveTemplate("TOKEN=private\nREMOVE=old", "TOKEN='[REDACTED]'\nNEW='[REDACTED]'\n")
	if err != nil {
		t.Fatal(err)
	}
	values, _ := Parse(got)
	if !reflect.DeepEqual(values, []string{"NEW=", "TOKEN=private"}) {
		t.Fatalf("values: %v", values)
	}
	for _, input := range []string{"TOKEN=plaintext", "TOKEN=", "INVALID", "TOKEN='unterminated"} {
		if _, err := ResolveTemplate("TOKEN=private", input); err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "unterminated") {
			t.Fatal("invalid template must fail without echoing input")
		}
	}
}

func TestSetValueRequiresExistingKeyAndQuotesLiteralValues(t *testing.T) {
	for _, value := range []string{"", "00123", "abc # def", "line1\nline2", "$TOKEN ${TOKEN}", `a"b'c`, `a\b`, "[REDACTED]"} {
		got, err := SetValue("TOKEN=old\nOTHER=keep", "TOKEN", value)
		if err != nil {
			t.Fatalf("set: %v", err)
		}
		values, err := Parse(got)
		if err != nil || !reflect.DeepEqual(values, []string{"OTHER=keep", "TOKEN=" + value}) {
			t.Fatalf("literal value did not round trip: %q, %v", value, err)
		}
	}
	for _, key := range []string{"UNKNOWN", "TOKEN\nOTHER", ""} {
		if _, err := SetValue("TOKEN=old", key, "new"); err == nil {
			t.Fatal("unknown/invalid key accepted")
		}
	}
}
