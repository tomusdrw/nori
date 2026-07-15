package crypto

import (
	"bytes"
	"testing"
)

func key32() []byte { return bytes.Repeat([]byte{7}, 32) }

func TestRoundTrip(t *testing.T) {
	pt := []byte("super-secret-value")
	ct, err := Encrypt(key32(), pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(ct, pt) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := Decrypt(key32(), ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestDecrypt_Tampered(t *testing.T) {
	ct, _ := Encrypt(key32(), []byte("x"))
	ct[len(ct)-1] ^= 0xFF
	if _, err := Decrypt(key32(), ct); err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	if _, err := Decrypt(key32(), []byte("short")); err == nil {
		t.Fatal("expected error on short ciphertext")
	}
}

func TestRejectsWrongKeyLength(t *testing.T) {
	key16 := bytes.Repeat([]byte{1}, 16)
	if _, err := Encrypt(key16, []byte("x")); err == nil {
		t.Fatal("expected error from Encrypt with 16-byte key")
	}
	if _, err := Decrypt(key16, bytes.Repeat([]byte{0}, 32)); err == nil {
		t.Fatal("expected error from Decrypt with 16-byte key")
	}
}
