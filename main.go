package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/pandada8/extended-ack-exteneded-network-controller/internal/controller"
	"github.com/pandada8/extended-ack-exteneded-network-controller/internal/natgw"
)

var scheme = runtimeScheme()

func runtimeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func main() {
	var metricsAddr string
	var probeAddr string
	var leaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&leaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	regionID := requiredEnv("ALIBABA_CLOUD_REGION_ID")
	natClient, err := natgw.NewAlibabaCloudClientFromEnv(regionID)
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to build NAT gateway client")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "pod-nat-eip-controller.network-x.alibabacloud-x.com",
	})
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.PodNATEIPReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		NATClient: natClient,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create controller", "controller", "PodNATEIP")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.WithName("setup").Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.WithName("setup").Error(err, "problem running manager")
		os.Exit(1)
	}
}

func requiredEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		ctrl.Log.WithName("setup").Error(nil, "missing required environment variable", "name", key)
		os.Exit(1)
	}
	return value
}
