package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDatabaseURLPrecedence(t *testing.T) {
	t.Setenv("EMBER_DATABASE_URL", "postgres://ember-environment")
	t.Setenv("DATABASE_URL", "postgres://platform-environment")
	path := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(path, []byte("postgres://file"), 0o600); err != nil {
		t.Fatalf("write database URL: %v", err)
	}

	value, err := resolveDatabaseURL("postgres://flag", path)
	if err != nil || value != "postgres://flag" {
		t.Fatalf("flag did not take precedence: value=%q err=%v", value, err)
	}

	value, err = resolveDatabaseURL("", path)
	if err != nil || value != "postgres://ember-environment" {
		t.Fatalf("Ember environment did not take precedence: value=%q err=%v", value, err)
	}

	t.Setenv("EMBER_DATABASE_URL", "")
	value, err = resolveDatabaseURL("", path)
	if err != nil || value != "postgres://platform-environment" {
		t.Fatalf("platform environment was not used: value=%q err=%v", value, err)
	}

	t.Setenv("DATABASE_URL", "")
	value, err = resolveDatabaseURL("", path)
	if err != nil || value != "postgres://file" {
		t.Fatalf("file fallback was not used: value=%q err=%v", value, err)
	}
}

func TestResolveDatabaseURLRequiresAConfiguredSource(t *testing.T) {
	t.Setenv("EMBER_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	_, err := resolveDatabaseURL("", "")
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("expected actionable missing database URL error, got %v", err)
	}
}

func TestLoadPrivateKeyUsesBase64EnvironmentValue(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	loaded, err := loadPrivateKey("/path/that/must/not/be/read", base64.StdEncoding.EncodeToString(privateKey))
	if err != nil {
		t.Fatalf("load encoded private key: %v", err)
	}
	if !bytes.Equal(loaded, privateKey) {
		t.Fatal("loaded private key does not match encoded input")
	}
}

func TestLoadPrivateKeyFallsBackToFile(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "private.key")
	if err := os.WriteFile(path, privateKey, 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	loaded, err := loadPrivateKey(path, "")
	if err != nil {
		t.Fatalf("load private key file: %v", err)
	}
	if !bytes.Equal(loaded, privateKey) {
		t.Fatal("loaded private key does not match file input")
	}
}

func TestLoadPrivateKeyRejectsInvalidBase64(t *testing.T) {
	_, err := loadPrivateKey("", "not-base64")
	if err == nil || !strings.Contains(err.Error(), "EMBER_GATEWAY_PRIVATE_KEY_BASE64") {
		t.Fatalf("expected base64 decoding error, got %v", err)
	}
}

func TestValidateReplitRuntime(t *testing.T) {
	for _, test := range []struct {
		name             string
		replitID         string
		gatewayURL       string
		privateKeyBase64 string
		secureCookies    bool
		wantError        string
	}{
		{
			name:          "local runtime keeps cluster defaults",
			gatewayURL:    defaultGatewayURL,
			secureCookies: false,
		},
		{
			name:             "Replit accepts HTTPS and secret-backed signing",
			replitID:         "app-id",
			gatewayURL:       "https://gateway.example.dev",
			privateKeyBase64: "configured",
			secureCookies:    true,
		},
		{
			name:             "Replit rejects plaintext gateway",
			replitID:         "app-id",
			gatewayURL:       "http://gateway.example.dev",
			privateKeyBase64: "configured",
			secureCookies:    true,
			wantError:        "https",
		},
		{
			name:          "Replit requires signing key secret",
			replitID:      "app-id",
			gatewayURL:    "https://gateway.example.dev",
			secureCookies: true,
			wantError:     "PRIVATE_KEY_BASE64",
		},
		{
			name:             "Replit requires secure cookies",
			replitID:         "app-id",
			gatewayURL:       "https://gateway.example.dev",
			privateKeyBase64: "configured",
			wantError:        "secure session cookies",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateReplitRuntime(test.replitID, test.gatewayURL, test.privateKeyBase64, test.secureCookies)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected error containing %q, got %v", test.wantError, err)
			}
		})
	}
}
