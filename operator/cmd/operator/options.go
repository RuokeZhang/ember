package main

import (
	"flag"
	"os"
	"strconv"

	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
)

type options struct {
	MetricsAddr    string
	ProbeAddr      string
	LeaderElection bool
	WatchNamespace string
	SimulationMode bool
	EnableKEDA     bool
	PrefetchImage  string
}

func parseOptions() options {
	opts := options{}
	flag.StringVar(&opts.MetricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&opts.ProbeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&opts.LeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	defaultNamespace := os.Getenv("EMBER_SYSTEM_NAMESPACE")
	if defaultNamespace == "" {
		defaultNamespace = servingv1alpha1.EmberSystemNamespace
	}
	flag.StringVar(&opts.WatchNamespace, "watch-namespace", defaultNamespace, "Namespace containing InferenceEndpoint CRs.")
	simulationMode, _ := strconv.ParseBool(os.Getenv("EMBER_SIMULATION_MODE"))
	flag.BoolVar(&opts.SimulationMode, "simulation-mode", simulationMode, "Use reduced CPU and memory requests while preserving GPU scheduling semantics.")
	enableKEDA, _ := strconv.ParseBool(os.Getenv("EMBER_ENABLE_KEDA"))
	flag.BoolVar(&opts.EnableKEDA, "enable-keda", enableKEDA, "Create KEDA ScaledObjects for endpoint Deployments.")
	defaultPrefetchImage := os.Getenv("EMBER_PREFETCH_IMAGE")
	if defaultPrefetchImage == "" {
		defaultPrefetchImage = "ember-prefetch:dev"
	}
	flag.StringVar(&opts.PrefetchImage, "prefetch-image", defaultPrefetchImage, "Image used for cache materialization and verification.")
	flag.Parse()
	return opts
}
