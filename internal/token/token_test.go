package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC)
	raw, err := Sign(privateKey, Claims{
		Subject:   "usr_31d2",
		Audience:  "ember-gateway",
		ID:        "req_123",
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	claims, err := Verify(publicKey, raw, "ember-gateway", now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "usr_31d2" || claims.ID != "req_123" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
}

func TestVerifyRejectsTamperingExpiryAndAudience(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC)
	raw, err := Sign(privateKey, Claims{
		Subject:   "usr_31d2",
		Audience:  "ember-gateway",
		ID:        "req_123",
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	parts := strings.Split(raw, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "A"
	if _, err := Verify(publicKey, strings.Join(parts, "."), "ember-gateway", now); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered token error = %v, want ErrSignature", err)
	}
	if _, err := Verify(publicKey, raw, "wrong-audience", now); !errors.Is(err, ErrAudience) {
		t.Fatalf("audience error = %v, want ErrAudience", err)
	}
	if _, err := Verify(publicKey, raw, "ember-gateway", now.Add(time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry error = %v, want ErrExpired", err)
	}
}

func TestSignRejectsExcessiveLifetime(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC)
	_, err = Sign(privateKey, Claims{
		Subject:   "usr_31d2",
		Audience:  "ember-gateway",
		ID:        "req_123",
		IssuedAt:  now,
		ExpiresAt: now.Add(MaxLifetime + time.Second),
	})
	if err == nil {
		t.Fatal("expected excessive lifetime rejection")
	}
}
