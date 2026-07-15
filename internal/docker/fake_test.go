package docker

import (
	"context"
	"testing"
)

func TestFake_ImplementsClient(t *testing.T) {
	var c Client = &Fake{Containers: map[string][]Container{
		"blog": {{Name: "blog-web", State: "running", Digest: "sha256:abc"}},
	}}
	got, err := c.ListByService(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "running" {
		t.Fatalf("unexpected: %+v", got)
	}
}
