package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
	"go.datum.net/ufo/internal/activator"
)

func main() {
	if err := run(); err != nil {
		slog.Error("ufo-activator exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		kubeconfig  string
		port        int
		metricsPort int
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file; uses in-cluster config when empty.")
	flag.IntVar(&port, "port", 8080, "Port to listen on for function traffic.")
	flag.IntVar(&metricsPort, "metrics-port", 9090, "Port to expose Prometheus metrics on.")
	flag.Parse()

	// Build Kubernetes REST config.
	cfg, err := buildRESTConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("build rest config: %w", err)
	}

	// Build scheme with the types the activator needs.
	scheme := runtime.NewScheme()
	if err := functionsv1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add functions scheme: %w", err)
	}
	if err := computev1alpha.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add compute scheme: %w", err)
	}

	// Build a cached client so Function/Workload reads are served from the
	// informer cache rather than hitting the API server on every request.
	ca, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create cache: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: ca}})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	// Register Prometheus metrics.
	reg := prometheus.NewRegistry()
	if err := activator.RegisterMetrics(reg); err != nil {
		return fmt.Errorf("register metrics: %w", err)
	}

	act := activator.New(c, ca)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the cache (informers).
	go func() {
		if err := ca.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("cache stopped with error", "err", err)
			stop()
		}
	}()

	// Start the activator index watcher.
	go func() {
		if err := act.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("activator index watcher stopped with error", "err", err)
			stop()
		}
	}()

	// Function traffic server.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", act)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Metrics server.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", metricsPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("ufo-activator metrics listening", "port", metricsPort)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server error", "err", err)
			stop()
		}
	}()

	go func() {
		slog.Info("ufo-activator listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("activator server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down ufo-activator")

	// Graceful shutdown: drain in-flight requests (up to 5 s).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("activator server shutdown error", "err", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("metrics server shutdown error", "err", err)
	}

	return nil
}

func buildRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return cfg, nil
}
