package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/RuokeZhang/ember/internal/token"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: ember-auth-tool <keygen|token|postgres-secret>")
	}
	switch os.Args[1] {
	case "keygen":
		runKeygen(os.Args[2:])
	case "token":
		runToken(os.Args[2:])
	case "postgres-secret":
		runPostgresSecret(os.Args[2:])
	default:
		fail("unknown command %q", os.Args[1])
	}
}

func runPostgresSecret(args []string) {
	flags := flag.NewFlagSet("postgres-secret", flag.ExitOnError)
	namespace := flags.String("namespace", "ember-system", "Secret namespace.")
	name := flags.String("name", "ember-postgres", "Secret name.")
	username := flags.String("username", "ember", "Postgres username.")
	database := flags.String("database", "ember", "Postgres database.")
	service := flags.String("service", "ember-postgres", "Postgres Service host.")
	_ = flags.Parse(args)

	if *username == "" || *database == "" || *service == "" {
		fail("username, database, and service are required")
	}
	var randomPassword [24]byte
	_, err := rand.Read(randomPassword[:])
	must(err)
	password := hex.EncodeToString(randomPassword[:])
	databaseURL := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", *username, password, *service, *database)
	fmt.Printf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: ember-postgres
    app.kubernetes.io/part-of: ember
type: Opaque
data:
  username: %s
  password: %s
  database: %s
  database-url: %s
`, *name, *namespace,
		base64.StdEncoding.EncodeToString([]byte(*username)),
		base64.StdEncoding.EncodeToString([]byte(password)),
		base64.StdEncoding.EncodeToString([]byte(*database)),
		base64.StdEncoding.EncodeToString([]byte(databaseURL)),
	)
}

func runKeygen(args []string) {
	flags := flag.NewFlagSet("keygen", flag.ExitOnError)
	namespace := flags.String("namespace", "ember-system", "Secret namespace.")
	name := flags.String("name", "ember-jwt-keys", "Secret name.")
	_ = flags.Parse(args)

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	fmt.Printf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: ember-jwt-keys
    app.kubernetes.io/part-of: ember
type: Opaque
data:
  public.key: %s
  private.key: %s
`, *name, *namespace, base64.StdEncoding.EncodeToString(publicKey), base64.StdEncoding.EncodeToString(privateKey))
}

func runToken(args []string) {
	flags := flag.NewFlagSet("token", flag.ExitOnError)
	privateKeyFile := flags.String("private-key-file", "", "Raw Ed25519 private key file.")
	privateKeyBase64Stdin := flags.Bool("private-key-base64-stdin", false, "Read a base64-encoded private key from stdin.")
	subject := flags.String("subject", "", "JWT subject.")
	audience := flags.String("audience", "ember-gateway", "JWT audience.")
	ttl := flags.Duration("ttl", 60*time.Second, "JWT lifetime.")
	_ = flags.Parse(args)

	var privateKeyBytes []byte
	var err error
	switch {
	case *privateKeyFile != "":
		privateKeyBytes, err = os.ReadFile(*privateKeyFile)
	case *privateKeyBase64Stdin:
		encoded, readErr := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if readErr != nil {
			err = readErr
			break
		}
		privateKeyBytes, err = base64.StdEncoding.DecodeString(string(encoded))
	default:
		fail("one private key input is required")
	}
	must(err)
	if len(privateKeyBytes) != ed25519.PrivateKeySize {
		fail("private key must be %d raw bytes, got %d", ed25519.PrivateKeySize, len(privateKeyBytes))
	}
	if *subject == "" {
		fail("subject is required")
	}
	var randomID [12]byte
	_, err = rand.Read(randomID[:])
	must(err)
	now := time.Now().UTC()
	raw, err := token.Sign(ed25519.PrivateKey(privateKeyBytes), token.Claims{
		Subject:   *subject,
		Audience:  *audience,
		ID:        hex.EncodeToString(randomID[:]),
		IssuedAt:  now,
		ExpiresAt: now.Add(*ttl),
	})
	must(err)
	fmt.Println(raw)
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
