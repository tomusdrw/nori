package envfile

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	got, err := Parse("# app config\nPORT=8080\nGREETING=\"hello world\"\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GREETING=hello world", "PORT=8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse() = %#v, want %#v", got, want)
	}
}

func TestParseRejectsInvalidFile(t *testing.T) {
	if _, err := Parse("NOT VALID"); err == nil {
		t.Fatal("expected invalid dotenv syntax to be rejected")
	}
	if _, err := Parse("1INVALID=value"); err == nil {
		t.Fatal("expected invalid variable name to be rejected")
	}
}
