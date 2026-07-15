package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashAndLogin(t *testing.T) {
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(hash, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword(a.passwordHash, []byte("secret")); err != nil {
		t.Fatal("password should match")
	}
	if err := bcrypt.CompareHashAndPassword(a.passwordHash, []byte("wrong")); err == nil {
		t.Fatal("wrong password should fail")
	}
}
