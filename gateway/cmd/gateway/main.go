package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/RuokeZhang/ember/gateway/internal/gateway"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	var listenAddress string
	var namespace string
	var publicKeyFile string
	var audience string
	var prometheusURL string
	flag.StringVar(&listenAddress, "listen-address", ":8080", "HTTP listen address.")
	flag.StringVar(&namespace, "namespace", servingv1alpha1.EmberSystemNamespace, "Namespace containing InferenceEndpoint resources.")
	flag.StringVar(&publicKeyFile, "public-key-file", "/var/run/ember/jwt/public.key", "Raw Ed25519 public key file.")
	flag.StringVar(&audience, "audience", gateway.DefaultAudience, "Required JWT audience.")
	flag.StringVar(&prometheusURL, "prometheus-url", "http://ember-prometheus.ember-system.svc.cluster.local:9090", "Prometheus base URL.")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	publicKey, err := loadPublicKey(publicKeyFile)
	must(err)

	scheme := clientgoscheme.Scheme
	must(servingv1alpha1.AddToScheme(scheme))
	config := ctrl.GetConfigOrDie()
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	must(err)
	coreClient, err := kubernetes.NewForConfig(config)
	must(err)
	store := gateway.NewKubernetesStore(kubeClient, coreClient, namespace)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 150 * time.Second,
	}
	metrics, err := gateway.NewPrometheusReader(prometheusURL, &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	})
	must(err)
	handler, err := gateway.NewServer(gateway.ServerOptions{
		Store:     store,
		Metrics:   metrics,
		PublicKey: publicKey,
		Audience:  audience,
		Logger:    logger,
		Transport: transport,
	})
	must(err)

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	logger.Info("Ember gateway listening", "address", listenAddress, "namespace", namespace)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		must(err)
	}
}

func loadPublicKey(path string) (ed25519.PublicKey, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	if len(value) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d raw bytes, got %d", ed25519.PublicKeySize, len(value))
	}
	return ed25519.PublicKey(value), nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
