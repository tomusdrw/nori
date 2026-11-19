package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"nori/internal/store"
)

func TestArchiveRoundTrip(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "database"), []byte("application data"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Service: store.Service{Name: "example"}, Env: "PASSWORD=hidden-value\n"}
	var encrypted bytes.Buffer
	if err := Encode(&encrypted, identity.Recipient(), manifest, source); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.Bytes(), []byte("hidden-value")) {
		t.Fatal("dotenv appears in ciphertext")
	}
	target := t.TempDir()
	got, err := Decode(bytes.NewReader(encrypted.Bytes()), identity, target, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "database"))
	if err != nil || string(data) != "application data" || got.Env != manifest.Env || got.Service.Name != "example" {
		t.Fatalf("unexpected restored contents: %q, %v", data, err)
	}
	wrong, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(bytes.NewReader(encrypted.Bytes()), wrong, t.TempDir(), 1024*1024); err == nil {
		t.Fatal("accepted the wrong recovery key")
	}
	truncated := encrypted.Bytes()[:encrypted.Len()-1]
	if _, err := Decode(bytes.NewReader(truncated), identity, t.TempDir(), 1024*1024); err == nil {
		t.Fatal("accepted truncated archive")
	}
}

func TestDecodeRejectsUnsafeOrRepeatedPayload(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"../payload", "payload"} {
		t.Run(invalid, func(t *testing.T) {
			var ciphertext bytes.Buffer
			encrypted, err := age.Encrypt(&ciphertext, identity.Recipient())
			if err != nil {
				t.Fatal(err)
			}
			zipped := gzip.NewWriter(encrypted)
			archive := tar.NewWriter(zipped)
			for _, name := range []string{"payload", invalid} {
				if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600}); err != nil {
					t.Fatal(err)
				}
			}
			if err := errors.Join(archive.Close(), zipped.Close(), encrypted.Close()); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			_, err = Decode(&ciphertext, identity, dest, 1024)
			if err == nil || (!strings.Contains(err.Error(), "invalid backup payload name") && !errors.Is(err, os.ErrExist)) {
				t.Fatalf("archive did not reject %q: %v", invalid, err)
			}
		})
	}
}
