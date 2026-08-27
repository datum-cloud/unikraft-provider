// Package stateprojector turns the Unikraft runtime's vm.state_change
// lifecycle stream into windowed, project-attributed usage records for
// per-second billing.
//
// The runtime (ukpd) is single-user and vendor-built: its events carry a
// runtime instance uuid and an old/new state, but no notion of a Datum
// project. This package bridges that gap on the node by resolving each
// uuid to a project/instance/resources via the provider Pod whose container
// status containerID matches it (see pod_index.go), then windowing the
// running time and appending one record per window to a JSONL file for a
// Vector agent to tail.
//
// Files are split by responsibility: namespace_index.go and pod_index.go
// build the attribution index off a Kubernetes watch (watch.go); window.go
// and processor.go turn events into windowed records; writer.go appends
// them and owns rotation; socket.go is today's event transport. It sits
// behind two interfaces (eventSource, which this file declares, and
// eventHandler, which socket.go declares), so a planned file-tailing
// source is a second implementation of eventSource — a change to this
// file's wiring in New only, nothing in processor.go or socket.go.
package stateprojector

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// eventSource drives an eventHandler with decoded events until ctx is done,
// owning any setup its transport needs. Satisfied by *socketSource today; a
// planned file-tailing source implements the same interface, so replacing
// one with the other only changes which constructor New calls below.
type eventSource interface {
	Run(ctx context.Context) error
}

// Config holds every user-facing setting; cmd/state-projector/main.go owns
// flag parsing and passes the result in here.
type Config struct {
	SocketPath      string
	OutputPath      string
	FlushInterval   time.Duration
	StatsInterval   time.Duration
	KubeconfigPath  string
	Debug           bool
	RotateSizeBytes int64
	RotateMaxAge    time.Duration
}

// Service is the running state-projector: a Kubernetes attribution watch, an
// event processor, and a socket listener, wired together.
type Service struct {
	cfg       Config
	clientset kubernetes.Interface

	namespaces *namespaceIndex
	pods       *podIndex
	proc       *processor
	source     eventSource
	stats      *stats
}

// New builds a Service from cfg, without starting anything yet.
func New(cfg Config) (*Service, error) {
	restCfg, err := k8sClientConfig(cfg.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}

	st := &stats{}
	namespaces := newNamespaceIndex()
	pods := newPodIndex(namespaces, st, cfg.Debug)
	out := newOutputWriter(cfg.OutputPath, cfg.RotateSizeBytes, cfg.RotateMaxAge, st)
	proc := newProcessor(pods, out, st, cfg.Debug, cfg.FlushInterval)
	source := newSocketSource(cfg.SocketPath, proc, st)

	return &Service{
		cfg:        cfg,
		clientset:  clientset,
		namespaces: namespaces,
		pods:       pods,
		proc:       proc,
		source:     source,
		stats:      st,
	}, nil
}

func k8sClientConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// Run starts the attribution watch, the periodic flush loop, the stats
// heartbeat, and finally blocks running the event source until ctx is done
// or it fails. The event source (socket today, a file-tailing one later)
// owns its own transport-specific setup.
func (s *Service) Run(ctx context.Context) error {
	defer runtime.HandleCrash()

	if err := os.MkdirAll(filepath.Dir(s.cfg.OutputPath), 0o755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}

	go watchCluster(ctx, s.clientset, s.pods, s.namespaces)
	go s.proc.periodicFlush(ctx)
	go logHeartbeat(ctx, s.stats, statsSource{
		indexedInstances: s.pods.len,
		openWindows:      s.proc.openWindows,
	}, s.cfg.StatsInterval)

	return s.source.Run(ctx)
}

// LogBoot logs the resolved configuration before anything can fail: a
// projector pointed at the wrong socket looks exactly like an idle one, and
// this line is what distinguishes them.
func (s *Service) LogBoot(hostname string) {
	authMode := "in-cluster"
	if s.cfg.KubeconfigPath != "" {
		authMode = "kubeconfig=" + s.cfg.KubeconfigPath
	}
	log.Printf("boot node=%s socket=%s output=%s flush_interval=%s stats_interval=%s auth=%s debug=%t on_state=%q rotate_size_mb=%d rotate_max_age=%s",
		hostname, s.cfg.SocketPath, s.cfg.OutputPath, s.cfg.FlushInterval, s.cfg.StatsInterval, authMode, s.cfg.Debug,
		onState, s.cfg.RotateSizeBytes/(1024*1024), s.cfg.RotateMaxAge)
}
