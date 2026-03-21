package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
	"easy-deploy/internal/controller"
	"easy-deploy/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(deployv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var healthAddr string
	var webhookAddr string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&webhookAddr, "webhook-bind-address", ":9090", "The address the GitHub webhook server binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for controller manager.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		HealthProbeBindAddress: healthAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "easy-deploy-operator.deploy.easydeploy.io",
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}
	//comment for trigger pipeline
	baseDomain := os.Getenv("BASE_DOMAIN")
	targetIP := os.Getenv("TARGET_IP")
	registryURL := os.Getenv("REGISTRY_URL")
	if baseDomain != "" {
		ctrl.Log.Info("auto-hostname enabled", "baseDomain", baseDomain, "targetIP", targetIP)
	}
	if registryURL != "" {
		ctrl.Log.Info("registry override enabled", "registryURL", registryURL)
	}

	if err := (&controller.BirServiceReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		BaseDomain:  baseDomain,
		TargetIP:    targetIP,
		RegistryURL: registryURL,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "BirService")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	go func() {
		wh := &webhook.GitHubHandler{Client: mgr.GetClient()}
		bc := &webhook.BuildCompleteHandler{Client: mgr.GetClient()}
		if err := webhook.StartServer(context.WithValue(ctx, struct{}{}, nil), webhookAddr, wh, bc); err != nil {
			ctrl.Log.Error(err, "webhook server failed")
		}
	}()

	ctrl.Log.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
