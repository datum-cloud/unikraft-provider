// Command state-projector is a per-node sidecar that turns the Unikraft
// runtime's vm.state_change lifecycle stream into windowed,
// project-attributed usage records for per-second billing. See
// internal/stateprojector for how it works.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"go.datum.net/unikraft-provider/internal/stateprojector"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)

	var (
		// /var/run/ukp, not /run/ukp: this ships on distroless/static, where
		// the two are separate real directories rather than a symlink pair,
		// and the ukp-run hostPath is mounted at /var/run/ukp.
		socketPath    = flag.String("socket", "/var/run/ukp/vm-state.sock", "unix socket path to listen on; ukpd's vm.state_change sink connects here")
		outputPath    = flag.String("output", "/var/run/ukp/vm-state.usage", "path to append windowed usage JSONL (must be on a hostPath Vector can read)")
		flushInterval = flag.Duration("flush-interval", 5*time.Minute, "how often open running windows are flushed as incremental records")
		statsInterval = flag.Duration("stats-interval", time.Minute, "how often the stats heartbeat line is logged")
		kubeconfig    = flag.String("kubeconfig", "", "optional path to a kubeconfig; defaults to in-cluster config")
		debug         = flag.Bool("debug", false, "log every event, attribution and pod-index change")
		rotateSizeMB  = flag.Int64("rotate-size-mb", 64, "rotate the output file once it reaches this size (megabytes)")
		rotateMaxAge  = flag.Duration("rotate-max-age", 48*time.Hour, "delete a rotated file once it is this old; a disk-safety backstop, not the primary cleanup path (that's Vector's)")
	)
	flag.Parse()

	svc, err := stateprojector.New(stateprojector.Config{
		SocketPath:      *socketPath,
		OutputPath:      *outputPath,
		FlushInterval:   *flushInterval,
		StatsInterval:   *statsInterval,
		KubeconfigPath:  *kubeconfig,
		Debug:           *debug,
		RotateSizeBytes: *rotateSizeMB * 1024 * 1024,
		RotateMaxAge:    *rotateMaxAge,
	})
	if err != nil {
		log.Fatalf("boot fatal=%v", err)
	}

	hostname, _ := os.Hostname()
	svc.LogBoot(hostname)

	if err := svc.Run(context.Background()); err != nil {
		log.Fatalf("run fatal=%v", err)
	}
}
