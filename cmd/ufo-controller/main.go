package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/cli"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	"go.datum.net/ufo/pkg/apis/functions/v1alpha1"
	functionctrl "go.datum.net/ufo/internal/controllers/function"
	functionbuildctrl "go.datum.net/ufo/internal/controllers/functionbuild"
	functionrevisionctrl "go.datum.net/ufo/internal/controllers/functionrevision"
	functionrevisiongcctrl "go.datum.net/ufo/internal/controllers/functionrevisiongc"

	k8sscheme "k8s.io/client-go/kubernetes/scheme"

	// Register JSON logging format.
	_ "k8s.io/component-base/logs/json/register"
)

func init() {
	klog.InitFlags(nil)
	utilruntime.Must(v1alpha1.AddToScheme(k8sscheme.Scheme))
	utilruntime.Must(computev1alpha.AddToScheme(k8sscheme.Scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(k8sscheme.Scheme))
	utilruntime.Must(batchv1.AddToScheme(k8sscheme.Scheme))
	utilruntime.Must(corev1.AddToScheme(k8sscheme.Scheme))
}

func main() {
	cmd := newRootCommand()
	code := cli.Run(cmd)
	os.Exit(code)
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ufo-controller",
		Short: "ufo-controller - controller manager for Unikraft Functions",
	}
	cmd.AddCommand(newRunCommand())
	return cmd
}

func newRunCommand() *cobra.Command {
	var metricsAddr string
	var probeAddr string
	var leaderElect bool
	var kraftImage string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the ufo controller manager",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runController(cmd.Context(), metricsAddr, probeAddr, leaderElect, kraftImage)
		},
	}

	cmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	cmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	cmd.Flags().BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for controller manager.")
	cmd.Flags().StringVar(&kraftImage, "kraft-image", "ghcr.io/unikraft/kraftkit:latest", "Container image used for kraft build jobs.")

	return cmd
}

func runController(ctx context.Context, metricsAddr, probeAddr string, leaderElect bool, kraftImage string) error {
	crlog.SetLogger(zap.New(zap.UseDevMode(true)))
	log := crlog.Log.WithName("ufo-controller")

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	scheme := k8sscheme.Scheme
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(computev1alpha.AddToScheme(scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "ufo-controller.functions.datumapis.com",
	})
	if err != nil {
		return fmt.Errorf("failed to create manager: %w", err)
	}

	if err := (&functionctrl.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up function controller: %w", err)
	}

	if err := (&functionrevisionctrl.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up function revision controller: %w", err)
	}

	if err := (&functionbuildctrl.Reconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		KraftImage: kraftImage,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up function build controller: %w", err)
	}

	if err := (&functionrevisiongcctrl.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up function revision gc controller: %w", err)
	}

	log.Info("all controllers registered")

	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		return fmt.Errorf("failed to start manager: %w", err)
	}

	return nil
}
