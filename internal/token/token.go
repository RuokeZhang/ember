package token

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const MaxLifetime = 2 * time.Minute

var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("invalid token signature")
	ErrExpired   = errors.New("token expired")
	ErrAudience  = errors.New("invalid token audience")
)

type Claims struct {
	Subject   string
	Audience  string
	ID        string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type wireClaims struct {
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	ID       string `json:"jti"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

func Sign(privateKey ed25519.PrivateKey, claims Claims) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("invalid Ed25519 private key")
	}
	if err := validateClaims(claims); err != nil {
		return "", err
	}

	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("encode token header: %w", err)
	}
	payload, err := json.Marshal(wireClaims{
		Subject:  claims.Subject,
		Audience: claims.Audience,
		ID:       claims.ID,
		IssuedAt: claims.IssuedAt.Unix(),
		Expires:  claims.ExpiresAt.Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("encode token claims: %w", err)
	}

	unsigned := encode(header) + "." + encode(payload)
	signature := ed25519.Sign(privateKey, []byte(unsigned))
	return unsigned + "." + encode(signature), nil
}

func Verify(publicKey ed25519.PublicKey, raw, expectedAudience string, now time.Time) (Claims, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return Claims{}, errors.New("invalid Ed25519 public key")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}

	headerBytes, err := decode(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if err := decodeStrict(headerBytes, &header); err != nil || header.Algorithm != "EdDSA" || header.Type != "JWT" {
		return Claims{}, ErrMalformed
	}

	signature, err := decode(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Claims{}, ErrMalformed
	}
	if !ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return Claims{}, ErrSignature
	}

	payload, err := decode(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var wire wireClaims
	if err := decodeStrict(payload, &wire); err != nil {
		return Claims{}, ErrMalformed
	}
	claims := Claims{
		Subject:   wire.Subject,
		Audience:  wire.Audience,
		ID:        wire.ID,
		IssuedAt:  time.Unix(wire.IssuedAt, 0).UTC(),
		ExpiresAt: time.Unix(wire.Expires, 0).UTC(),
	}
	if err := validateClaims(claims); err != nil {
		return Claims{}, ErrMalformed
	}
	if claims.Audience != expectedAudience {
		return Claims{}, ErrAudience
	}
	now = now.UTC()
	if !claims.IssuedAt.Before(claims.ExpiresAt) || claims.ExpiresAt.Sub(claims.IssuedAt) > MaxLifetime {
		return Claims{}, ErrMalformed
	}
	if !now.Before(claims.ExpiresAt) {
		return Claims{}, ErrExpired
	}
	if claims.IssuedAt.After(now.Add(5 * time.Second)) {
		return Claims{}, ErrMalformed
	}
	return claims, nil
}

func validateClaims(claims Claims) error {
	if strings.TrimSpace(claims.Subject) == "" {
		return errors.New("token subject is required")
	}
	if strings.TrimSpace(claims.Audience) == "" {
		return errors.New("token audience is required")
	}
	if strings.TrimSpace(claims.ID) == "" {
		return errors.New("token ID is required")
	}
	if claims.IssuedAt.IsZero() || claims.ExpiresAt.IsZero() {
		return errors.New("token timestamps are required")
	}
	if !claims.ExpiresAt.After(claims.IssuedAt) {
		return errors.New("token expiry must follow issue time")
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt) > MaxLifetime {
		return fmt.Errorf("token lifetime exceeds %s", MaxLifetime)
	}
	return nil
}

func encode(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func decode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}

func decodeStrict(value []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrMalformed
	}
	return nil
}
