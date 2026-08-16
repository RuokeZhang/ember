package main

import (
	"fmt"
	"os"

	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/RuokeZhang/ember/operator/controllers"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	opts := parseOptions()
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := clientgoscheme.Scheme
	must(clientgoscheme.AddToScheme(scheme))
	must(appsv1.AddToScheme(scheme))
	must(batchv1.AddToScheme(scheme))
	must(networkingv1.AddToScheme(scheme))
	must(rbacv1.AddToScheme(scheme))
	must(corev1.AddToScheme(scheme))
	must(servingv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			ByObject: map[crclient.Object]cache.ByObject{
				&servingv1alpha1.InferenceEndpoint{}: {Namespaces: map[string]cache.Config{opts.WatchNamespace: {}}},
			},
		},
		Metrics:                server.Options{BindAddress: opts.MetricsAddr},
		HealthProbeBindAddress: opts.ProbeAddr,
		LeaderElection:         opts.LeaderElection,
		LeaderElectionID:       "ember-operator-serving.ember.dev",
	})
	must(err)

	directClient, err := crclient.New(ctrl.GetConfigOrDie(), crclient.Options{Scheme: scheme})
	must(err)

	endpointReconciler := &controllers.EndpointReconciler{Client: mgr.GetClient(), DirectClient: directClient, APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), ManagedNamespace: opts.WatchNamespace, SimulationMode: opts.SimulationMode, EnableKEDA: opts.EnableKEDA}
	must(endpointReconciler.SetupWithManager(mgr))
	modelCacheReconciler := &controllers.ModelCacheReconciler{Client: mgr.GetClient(), DirectClient: directClient, APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), ManagedNamespace: opts.WatchNamespace, SimulationMode: opts.SimulationMode}
	must(modelCacheReconciler.SetupWithManager(mgr))
	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
