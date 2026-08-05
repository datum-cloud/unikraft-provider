package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.datum.net/unikraft-provider/internal/ukpmetrics"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var (
		listenAddr      string
		ukpdURL         string
		ukpdToken       string
		ukpdTokenFile   string
		ukpdUsersFile   string
		platformDir     string
		runtimeNodeName string
		virtualNodeName string
		kubeconfig      string
	)

	flag.StringVar(&listenAddr, "listen-address", ":9102", "Address to listen on.")
	flag.StringVar(&ukpdURL, "ukpd-url", "http://127.0.0.1:45232", "Local ukpd API URL.")
	flag.StringVar(&ukpdToken, "ukpd-token", env("UKPD_TOKEN", env("KRAFTLET_UKC_TOKEN", "")), "Bearer token for the ukpd API. Defaults to UKPD_TOKEN or KRAFTLET_UKC_TOKEN.")
	flag.StringVar(&ukpdTokenFile, "ukpd-token-file", "", "File containing the bearer token for the ukpd API.")
	flag.StringVar(&ukpdUsersFile, "ukpd-users-file", "/var/lib/ukp/data/users.json", "ukpd users.json file to read a bearer token from when no token is configured.")
	flag.StringVar(&platformDir, "platform-dir", "/var/lib/ukp/data/platform", "ukpd platform workspace directory, used as a fallback source for guest IPs.")
	flag.StringVar(&runtimeNodeName, "runtime-node-name", env("NODE_NAME", ""), "Real Kubernetes node name running this exporter.")
	flag.StringVar(&virtualNodeName, "virtual-node-name", "", "Kraftlet virtual node name to match Pods against. Defaults to kraftlet-<runtime-node-name> when runtime-node-name is set.")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig. If empty, in-cluster config is used.")
	flag.Parse()

	if ukpdTokenFile != "" {
		data, err := os.ReadFile(ukpdTokenFile)
		if err != nil {
			log.Fatalf("read ukpd token file: %v", err)
		}
		ukpdToken = strings.TrimSpace(string(data))
	}
	if ukpdToken == "" && ukpdUsersFile != "" {
		var err error
		ukpdToken, err = tokenFromUsersFile(ukpdUsersFile)
		if err != nil {
			log.Printf("unable to read ukpd token from users file: %v", err)
		}
	}
	if virtualNodeName == "" && runtimeNodeName != "" {
		virtualNodeName = "kraftlet-" + runtimeNodeName
	}

	restConfig, err := kubeRESTConfig(kubeconfig)
	if err != nil {
		log.Fatalf("build kubernetes config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("build kubernetes client: %v", err)
	}

	exporter, err := ukpmetrics.NewExporter(ukpmetrics.Config{
		UKPClient:       ukpmetrics.NewSDKClient(ukpdURL, ukpdToken),
		PlatformDir:     platformDir,
		RuntimeNodeName: runtimeNodeName,
		VirtualNodeName: virtualNodeName,
		KubeClient:      kubeClient,
	})
	if err != nil {
		log.Fatalf("create exporter: %v", err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(ukpmetrics.NewCollector(exporter))

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })

	log.Printf("starting ukp resource metrics exporter on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func kubeRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func tokenFromUsersFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var users []struct {
		AuthToken string `json:"auth_token"`
	}
	if err := json.Unmarshal(data, &users); err != nil {
		return "", err
	}
	for _, user := range users {
		if user.AuthToken != "" {
			return user.AuthToken, nil
		}
	}
	return "", fmt.Errorf("no auth_token entries found")
}
