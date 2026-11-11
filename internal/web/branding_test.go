package web

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"nori/internal/auth"
	"nori/internal/docker"
	"nori/internal/store"
)

func TestBrandAssetsAvailableBeforeLogin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "branding.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hash, err := auth.HashPassword("test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(hash, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(st, &docker.Fake{}, nil, nil, a, Channels{})

	for _, tc := range []struct {
		path string
		size int
	}{
		{"/static/nori-mark.png", 192},
		{"/static/favicon-32.png", 32},
		{"/static/apple-touch-icon.png", 180},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("unauthenticated asset status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "image/png" {
				t.Fatalf("asset content type = %q, want image/png", got)
			}
			img, err := png.Decode(rec.Body)
			if err != nil {
				t.Fatalf("decode delivered logo: %v", err)
			}
			if b := img.Bounds(); b.Dx() != tc.size || b.Dy() != tc.size {
				t.Fatalf("logo dimensions = %v, want %dx%d", b, tc.size, tc.size)
			}

		})
	}
}

func TestBrandingPagesReferenceTheMascotAndIcons(t *testing.T) {
	for _, tc := range []struct {
		name string
		page templ.Component
	}{
		{"dashboard", Page(nil, "csrf")},
		{"login", LoginPage("", "", "/")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var page bytes.Buffer
			if err := tc.page.Render(context.Background(), &page); err != nil {
				t.Fatal(err)
			}
			for _, reference := range []string{
				`src="/static/nori-mark.png" alt=""`,
				`href="/static/favicon-32.png" type="image/png" sizes="32x32"`,
				`rel="apple-touch-icon" href="/static/apple-touch-icon.png" sizes="180x180"`,
			} {
				if !strings.Contains(page.String(), reference) {
					t.Fatalf("rendered page is missing logo reference %q", reference)
				}
			}
		})
	}
}
