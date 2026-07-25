package docker

import (
	"context"
	"testing"
)

func TestFake_ImplementsClient(t *testing.T) {
	var c Client = &Fake{Containers: map[string][]Container{
		// Populate with a Health state and ExitCode for the test coverage of new fields
		"blog": {{Name: "blog-web", State: "running", Digest: "sha256:abc", Health: "healthy", ExitCode: 0}},
	}}
	got, err := c.ListByService(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "running" {
		t.Fatalf("unexpected: %+v", got)
	}
	// Ensure new fields are present and populated in the fake path
	if got[0].Health != "healthy" {
		t.Fatalf("expected Health to be 'healthy', got %q", got[0].Health)
	}
	if got[0].ExitCode != 0 {
		t.Fatalf("expected ExitCode to be 0, got %d", got[0].ExitCode)
	}
}
