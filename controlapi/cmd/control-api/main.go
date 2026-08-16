package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RuokeZhang/ember/controlapi/internal/controlapi"
	"github.com/RuokeZhang/ember/controlapi/internal/postgres"
)

func main() {
	var listenAddress string
	var databaseURL string
	var databaseURLFile string
	var gatewayURL string
	var privateKeyFile string
	var gatewayAudience string
	var sessionTTL time.Duration
	var secureCookies bool
	var webRoot string
	flag.StringVar(&listenAddress, "listen-address", ":8080", "HTTP listen address.")
	flag.StringVar(&databaseURL, "database-url", "", "Postgres connection URL.")
	flag.StringVar(&databaseURLFile, "database-url-file", "", "File containing the Postgres connection URL.")
	flag.StringVar(&gatewayURL, "gateway-url", "http://ember-gateway.ember-system.svc.cluster.local:8080", "Endpoint Gateway base URL.")
	flag.StringVar(&privateKeyFile, "private-key-file", "/var/run/ember/jwt/private.key", "Raw Ed25519 private key file.")
	flag.StringVar(&gatewayAudience, "gateway-audience", "ember-gateway", "Gateway JWT audience.")
	flag.DurationVar(&sessionTTL, "session-ttl", 24*time.Hour, "Demo session lifetime.")
	flag.BoolVar(&secureCookies, "secure-cookies", true, "Mark session cookies Secure.")
	flag.StringVar(&webRoot, "web-root", "", "Directory containing the built Ember web application.")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	resolvedDatabaseURL, err := resolveSecretValue(databaseURL, databaseURLFile, "EMBER_DATABASE_URL")
	must(err)
	privateKey, err := loadPrivateKey(privateKeyFile)
	must(err)

	store, err := postgres.Open(ctx, resolvedDatabaseURL)
	must(err)
	defer store.Close()
	must(store.Migrate(ctx))

	gatewayClient, err := controlapi.NewGatewayClient(controlapi.GatewayClientOptions{
		BaseURL:    gatewayURL,
		PrivateKey: privateKey,
		Audience:   gatewayAudience,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 150 * time.Second,
			},
		},
	})
	must(err)
	apiHandler, err := controlapi.NewServer(controlapi.ServerOptions{
		Store:         store,
		Gateway:       gatewayClient,
		Logger:        logger,
		SessionTTL:    sessionTTL,
		SecureCookies: secureCookies,
	})
	must(err)
	var handler http.Handler = apiHandler
	if strings.TrimSpace(webRoot) != "" {
		handler, err = controlapi.NewWebHandler(apiHandler, os.DirFS(webRoot))
		must(err)
	}

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("control API shutdown failed", "error", err)
		}
	}()

	logger.Info("Ember control API listening", "address", listenAddress, "gateway", gatewayURL)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		must(err)
	}
}

func resolveSecretValue(flagValue, filePath, environmentName string) (string, error) {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value, nil
	}
	if value := strings.TrimSpace(os.Getenv(environmentName)); value != "" {
		return value, nil
	}
	if strings.TrimSpace(filePath) == "" {
		return "", fmt.Errorf("one of --database-url, --database-url-file, or %s is required", environmentName)
	}
	value, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read database URL file: %w", err)
	}
	if strings.TrimSpace(string(value)) == "" {
		return "", errors.New("database URL is empty")
	}
	return strings.TrimSpace(string(value)), nil
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	if len(value) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key must be %d raw bytes, got %d", ed25519.PrivateKeySize, len(value))
	}
	return ed25519.PrivateKey(value), nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
