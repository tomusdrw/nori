package registry

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestLatestDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/team/app:latest"

	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatalf("push: %v", err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	got, err := LatestDigest(ref, crane.Insecure)
	if err != nil {
		t.Fatalf("LatestDigest: %v", err)
	}
	if got != want.String() {
		t.Fatalf("digest = %s, want %s", got, want.String())
	}
}
