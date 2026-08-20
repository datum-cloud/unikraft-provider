// Command state-projector is a per-node sidecar that turns the Unikraft runtime's
// vm.state_change lifecycle stream into windowed, project-attributed usage records
// for per-second billing.
//
// The runtime (ukpd) is single-user and vendor-built: its vm.state_change events
// carry a runtime instance uuid and an old/new state, but no notion of a Datum
// project. The projector bridges that gap on the node:
//
//	uuid -> guest IP   from the per-instance vmm.json (same parse as ukp-telemetry's
//	                   ip-surfacer; guest IP is the provider Pod's podIP)
//	guest IP -> project / instance / requested resources
//	                   from a cluster-wide watch of provider Pods
//
// For each instance it maintains an open "running window" and, on scale-to-zero
// (STANDBY), stop, or a periodic flush, appends a windowed usage record (JSONL) to
// the ukp-run hostPath for the billing-usage-collector Vector agent to tail:
//
//	{"id":<md5(uuid|start|end)>,"project":"<ns>","instance":"<name>","uuid":"...",
//	 "vcpu":N,"memory_bytes":M,"start":"...","end":"...","duration_s":N.N}
//
// The id is deterministic per (uuid, start, end), so replaying (or the runtime's
// reliable sink buffer retrying) cannot double-count a window downstream.
//
// This is the runtime-side half of per-second usage billing; see the compute
// enhancement docs/enhancements/per-second-usage-billing/README.md.
//
// # Logs
//
// Every failure mode here is silent by nature — an event whose state fields we
// cannot read simply produces no record, which looks identical to an idle node.
// So each stage logs a greppable "tag key=value" line, and a "stats" heartbeat
// summarizes counters on an interval. Tags: boot, podwatch, podindex, conn,
// event, window, resolve, record, stats. Start with the stats line to see
// whether events are arriving and records are being written at all; -debug adds
// per-event and per-pod detail.
package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// upstreamInstanceLabel is stamped on the provider Pod (and its Service) by the
// Instance controller; it names the compute Instance the pod backs.
const upstreamInstanceLabel = "upstream.instance"

// upstreamClusterNameLabel is stamped on the edge Namespace — never the Pod;
// this repo's own instance_controller_singleclient_test.go asserts the Pod
// must NOT carry it, calling that "the old multi-cluster routing mechanism
// that caused BUG-1" — by Karmada federation (go.datum.net/compute's NSO
// MappedNamespaceResourceStrategy) before any Instance can exist in that
// namespace. Its value decodes to the real Milo project id.
//
// The namespace's own name (ns-<uuid>) is a synthetic, edge-local identifier
// only — go.datum.net/compute's own controller comments say so explicitly
// ("does not exist in the project control plane") — NOT the project. Using
// pod.Namespace directly as the billing "project" was a real bug: verified
// against a live deployment, label value "cluster-project-htxrg" on namespace
// "ns-7c30e6d4-..." decodes to project "project-htxrg", the actual project
// name — nothing like the namespace's own name.
//
// Mirrored here (key + decode logic) rather than importing
// go.miloapis.com/milo/pkg/downstreamclient and go.datum.net/compute's
// internal/controller/clustername.go, to avoid pulling their much heavier
// transitive dependency trees (multicluster-runtime, Karmada client, etc.)
// into this otherwise-minimal static binary for two string constants and a
// one-line decode.
const upstreamClusterNameLabel = "meta.datumapis.com/upstream-cluster-name"

// decodeProjectID reverses go.datum.net/compute's EncodeClusterName
// ("cluster-" + strings.ReplaceAll(name, "/", "_")), returning the real Milo
// project id from an upstreamClusterNameLabel value.
func decodeProjectID(encoded string) string {
	return strings.ReplaceAll(strings.TrimPrefix(encoded, "cluster-"), "_", "/")
}

// onState is the single runtime state that counts as "running/consuming". Every
// other state (standby, stopped, stopping, draining, terminated) is "off" and
// bills nothing.
const onState = "running"

// maxEventSkew is how far an event's own timestamp may sit behind wall clock
// before it is called out. ukpd and this sidecar share the node clock, so any
// real skew means a replayed/buffered event — which matters because flushes are
// computed against wall clock (see periodicFlush).
const maxEventSkew = time.Minute

// Why an instance could not be attributed to a project. Each points at a
// different root cause, so they are logged distinctly rather than as one
// "unresolved" catch-all.
const (
	reasonOK            = "ok"
	reasonVMMUnreadble  = "vmm_json_unreadable" // wrong platform-dir, or ukpd has not written it yet
	reasonNoNetdevIP    = "netdev_ip_missing"   // vmm.json exists but carries no netdev.ip=
	reasonPodNotIndexed = "pod_not_indexed"     // guest IP resolved, but no provider Pod has that podIP
)

var (
	// Parse the per-instance vmm.json (Firecracker config) for the guest's IP.
	// vmm.json is itself JSON, and its boot_args string value wraps the netdev
	// arg in escaped double quotes, so the raw bytes read
	// netdev.ip=\"172.16.0.5/30:172.16.0.6:172.16.0.6::<hostname>:internal\" —
	// confirmed against a real vmm.json from ukpd (not just mirrored from
	// ukp-telemetry's ip-surfacer.sh, which isn't in this repo and turned out
	// not to match). The optional `\"` / `"` are skipped rather than required,
	// so an unescaped or differently-quoted variant still matches; the IP
	// itself is the first dotted-quad, before the /<prefix>.
	netdevIPRe = regexp.MustCompile(`netdev\.ip=\\?"?([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)`)

	// uidFieldRe is used to scan for the instance uuid if the event does not carry
	// a uuid at the top level of data.
	uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// stateChange is the subset of a vm.state_change event (JSON encoding) we care
// about. json.Decoder tolerates unknown fields, so the full vendor payload is
// dropped; only these are read.
type stateChange struct {
	Timestamp string                 `json:"timestamp"`
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`

	// Raw is the exact wire bytes, kept only so a payload we fail to interpret
	// can be logged verbatim — the vendor's key names are the thing most likely
	// to drift, and a paraphrase of the payload would hide exactly that.
	Raw json.RawMessage `json:"-"`
}

// usageRecord is one windowed billing record written to the output JSONL stream.
type usageRecord struct {
	ID          string  `json:"id"`
	Project     string  `json:"project"`
	Instance    string  `json:"instance"`
	UUID        string  `json:"uuid"`
	VCPU        int64   `json:"vcpu"`
	MemoryBytes int64   `json:"memory_bytes"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	DurationS   float64 `json:"duration_s"`
}

// podInfo is the identity + requested resources of the provider Pod for an
// instance, indexed by its podIP (the guest's IP).
type podInfo struct {
	project     string
	instance    string
	vcpuMilli   int64 // millicores; 0 if unset
	memoryBytes int64
}

func (p *podInfo) equal(other *podInfo) bool {
	if p == nil || other == nil {
		return p == other
	}
	return *p == *other
}

func (p *podInfo) String() string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("project=%s instance=%s vcpu_milli=%d memory_bytes=%d",
		p.project, p.instance, p.vcpuMilli, p.memoryBytes)
}

// windowState is the meter's per-instance running-window bookkeeping.
type windowState struct {
	uuid          string
	runningSince  time.Time // when the current running stretch began (window identity)
	reportedUntil time.Time // watermark of the last emitted record for this window
	resolved      *podInfo

	records        int    // records emitted for this window, for the close log
	lastResolveLog string // last attribution failure logged, to avoid repeating it every flush
}

// stats are cumulative counters reported by the heartbeat. They exist so a
// single log line can answer "is anything arriving, and is anything coming out"
// without correlating a whole log stream.
type stats struct {
	connections         atomic.Int64
	decodeErrors        atomic.Int64
	eventsReceived      atomic.Int64
	eventsWrongTyp      atomic.Int64
	noUUID              atomic.Int64
	noState             atomic.Int64
	windowsOpened       atomic.Int64
	windowsClosed       atomic.Int64
	recordsWritten      atomic.Int64
	writeErrors         atomic.Int64
	unresolved          atomic.Int64
	podIndexed          atomic.Int64
	watchErrors         atomic.Int64
	staleEvents         atomic.Int64
	overbilled          atomic.Int64
	rotations           atomic.Int64
	rotationDeletes     atomic.Int64
	projectLabelMissing atomic.Int64
}

type projector struct {
	platformDir     string
	socketPath      string
	outputPath      string
	flushInterval   time.Duration
	debug           bool
	rotateSizeBytes int64
	rotateMaxAge    time.Duration

	stats stats

	// resolved pods: guest IP (podIP) -> info. Guarded by podMu.
	mu   sync.RWMutex
	pods map[string]*podInfo

	// namespace name -> decoded Milo project id. See upstreamClusterNameLabel.
	nsMu     sync.RWMutex
	projects map[string]string

	// per-instance running windows.
	winMu   sync.Mutex
	windows map[string]*windowState
}

// debugf logs only under -debug: per-event and per-pod detail useful when
// chasing a specific instance, too chatty to leave on.
func (p *projector) debugf(format string, args ...interface{}) {
	if p.debug {
		log.Printf(format, args...)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	defer runtime.HandleCrash()

	var (
		// /var/run/ukp, not /run/ukp: this ships on distroless/static, where the
		// two are separate real directories rather than a symlink pair, and the
		// ukp-run hostPath is mounted at /var/run/ukp. Naming /run/ukp here
		// would silently place the socket and usage file in the container's
		// ephemeral layer, where ukpd and Vector cannot reach them.
		socketPath    = flag.String("socket", "/var/run/ukp/vm-state.sock", "unix socket path to listen on; ukpd's vm.state_change sink connects here")
		outputPath    = flag.String("output", "/var/run/ukp/vm-state.usage", "path to append windowed usage JSONL (must be on a hostPath Vector can read)")
		platformDir   = flag.String("platform-dir", "/var/lib/ukp/data/platform", "ukpd per-instance workspace dir (holds <uuid>/vmm.json)")
		flushInterval = flag.Duration("flush-interval", 5*time.Minute, "how often open running windows are flushed as incremental records")
		statsInterval = flag.Duration("stats-interval", time.Minute, "how often the stats heartbeat line is logged")
		kubeconfig    = flag.String("kubeconfig", "", "optional path to a kubeconfig; defaults to in-cluster config")
		debug         = flag.Bool("debug", false, "log every event, attribution and pod-index change")
		// Rotation: state-projector owns rotating the output file (rename, never
		// truncate in place — a reader with the file already open keeps reading
		// the renamed file to its final EOF undisturbed). Deletion of rotated
		// files is properly Vector's job once it exists (only it knows a file
		// was fully shipped); -rotate-max-age is a backstop for the gap before
		// that exists, or if Vector is ever down longer than the window — not
		// the primary cleanup path. See cmd/state-projector/README.md.
		rotateSizeMB = flag.Int64("rotate-size-mb", 64, "rotate the output file once it reaches this size (megabytes)")
		rotateMaxAge = flag.Duration("rotate-max-age", 48*time.Hour, "delete a rotated file once it is this old; a disk-safety backstop, not the primary cleanup path (that's Vector's)")
	)
	flag.Parse()

	// Log the resolved configuration before anything can fail: a projector
	// pointed at the wrong socket or platform-dir looks exactly like an idle
	// one, and this line is what distinguishes them.
	hostname, _ := os.Hostname()
	authMode := "in-cluster"
	if *kubeconfig != "" {
		authMode = "kubeconfig=" + *kubeconfig
	}
	log.Printf("boot node=%s socket=%s output=%s platform_dir=%s flush_interval=%s stats_interval=%s auth=%s debug=%t on_state=%q rotate_size_mb=%d rotate_max_age=%s",
		hostname, *socketPath, *outputPath, *platformDir, *flushInterval, *statsInterval, authMode, *debug, onState, *rotateSizeMB, *rotateMaxAge)

	cfg, err := k8sClientConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("boot fatal=k8s_config err=%v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("boot fatal=k8s_client err=%v", err)
	}

	p := &projector{
		platformDir:     *platformDir,
		socketPath:      *socketPath,
		outputPath:      *outputPath,
		flushInterval:   *flushInterval,
		debug:           *debug,
		rotateSizeBytes: *rotateSizeMB * 1024 * 1024,
		rotateMaxAge:    *rotateMaxAge,
		pods:            make(map[string]*podInfo),
		projects:        make(map[string]string),
		windows:         make(map[string]*windowState),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o755); err != nil {
		log.Fatalf("boot fatal=mkdir_socket_dir dir=%s err=%v", filepath.Dir(*socketPath), err)
	}
	if err := os.MkdirAll(filepath.Dir(*outputPath), 0o755); err != nil {
		log.Fatalf("boot fatal=mkdir_output_dir dir=%s err=%v", filepath.Dir(*outputPath), err)
	}
	// Surface the platform dir's readability now rather than as a per-event
	// attribution failure later.
	if _, err := os.Stat(*platformDir); err != nil {
		log.Printf("boot warn=platform_dir_unreadable dir=%s err=%v (attribution will fail until ukpd creates it)", *platformDir, err)
	}

	go p.watchPods(ctx, clientset)
	go p.periodicFlush(ctx)
	go p.logStats(ctx, *statsInterval)

	if err := p.listenAndServe(ctx); err != nil {
		log.Fatalf("listen fatal=%v", err)
	}
}

func k8sClientConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// watchPods maintains two indexes off a single shared informer factory:
// guest-IP -> (project, instance, resources), from a cluster-wide watch of
// provider Pods (they live on the Kraftlet virtual node, so node-scoping
// cannot apply — same constraint ukp-telemetry's k8sattributes has); and
// namespace -> decoded Milo project id, from a cluster-wide watch of
// Namespaces (see upstreamClusterNameLabel) — the project a Pod belongs to is
// not derivable from the Pod alone.
func (p *projector) watchPods(ctx context.Context, clientset *kubernetes.Clientset) {
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	podInformer := factory.Core().V1().Pods().Informer()
	nsInformer := factory.Core().V1().Namespaces().Informer()

	// A denied watch (missing RBAC) would otherwise show up only as every
	// instance being unattributable, so name it at the source.
	watchErrHandler := func(_ context.Context, _ *cache.Reflector, err error) {
		p.stats.watchErrors.Add(1)
		log.Printf("podwatch error=%v (check the ukp-state-projector-pod-reader ClusterRole/Binding)", err)
	}
	if err := podInformer.SetWatchErrorHandlerWithContext(watchErrHandler); err != nil {
		log.Printf("podwatch warn=set_error_handler err=%v", err)
	}
	if err := nsInformer.SetWatchErrorHandlerWithContext(watchErrHandler); err != nil {
		log.Printf("podwatch warn=set_error_handler err=%v", err)
	}

	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { p.upsertPod(toPod(obj)) },
		UpdateFunc: func(_, newObj interface{}) { p.upsertPod(toPod(newObj)) },
		DeleteFunc: func(obj interface{}) { p.deletePod(toPod(obj)) },
	})
	if err != nil {
		log.Fatalf("podwatch fatal=add_event_handler err=%v", err)
	}
	_, err = nsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { p.upsertNamespace(toNamespace(obj)) },
		UpdateFunc: func(_, newObj interface{}) { p.upsertNamespace(toNamespace(newObj)) },
		DeleteFunc: func(obj interface{}) { p.deleteNamespace(toNamespace(obj)) },
	})
	if err != nil {
		log.Fatalf("podwatch fatal=add_event_handler err=%v", err)
	}

	// Report when the index is actually usable. Until this line appears,
	// attribution failures are expected rather than a misconfiguration.
	go func() {
		if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced, nsInformer.HasSynced) {
			log.Printf("podwatch warn=cache_sync_aborted")
			return
		}
		p.mu.RLock()
		indexed := len(p.pods)
		p.mu.RUnlock()
		p.nsMu.RLock()
		nsIndexed := len(p.projects)
		p.nsMu.RUnlock()
		log.Printf("podwatch synced indexed_pod_ips=%d indexed_projects=%d", indexed, nsIndexed)
	}()

	log.Printf("podwatch starting scope=cluster-wide resync=30s")
	factory.Start(ctx.Done())
	<-ctx.Done()
	log.Printf("podwatch stopped")
}

func toPod(obj interface{}) *corev1.Pod {
	switch t := obj.(type) {
	case *corev1.Pod:
		return t
	case cache.DeletedFinalStateUnknown:
		if pod, ok := t.Obj.(*corev1.Pod); ok {
			return pod
		}
	}
	return nil
}

func toNamespace(obj interface{}) *corev1.Namespace {
	switch t := obj.(type) {
	case *corev1.Namespace:
		return t
	case cache.DeletedFinalStateUnknown:
		if ns, ok := t.Obj.(*corev1.Namespace); ok {
			return ns
		}
	}
	return nil
}

// upsertNamespace caches the decoded Milo project id for a namespace, read
// from upstreamClusterNameLabel. A namespace with no such label (most of the
// cluster — this is stamped only on edge namespaces federated for a project,
// not e.g. kube-system) simply has no entry; projectForNamespace's ok=false
// return distinguishes "not a project namespace" from "resolved".
func (p *projector) upsertNamespace(ns *corev1.Namespace) {
	if ns == nil {
		return
	}
	encoded, ok := ns.Labels[upstreamClusterNameLabel]
	p.nsMu.Lock()
	if ok {
		p.projects[ns.Name] = decodeProjectID(encoded)
	} else {
		delete(p.projects, ns.Name)
	}
	p.nsMu.Unlock()
}

func (p *projector) deleteNamespace(ns *corev1.Namespace) {
	if ns == nil {
		return
	}
	p.nsMu.Lock()
	delete(p.projects, ns.Name)
	p.nsMu.Unlock()
}

// projectForNamespace returns the decoded Milo project id for a namespace, and
// whether it resolved. false means either the namespace hasn't been indexed
// yet, or it genuinely carries no upstreamClusterNameLabel.
func (p *projector) projectForNamespace(ns string) (string, bool) {
	p.nsMu.RLock()
	defer p.nsMu.RUnlock()
	project, ok := p.projects[ns]
	return project, ok
}

func (p *projector) upsertPod(pod *corev1.Pod) {
	if pod == nil || pod.Status.PodIP == "" {
		return
	}
	instance := pod.Labels[upstreamInstanceLabel]
	// project is "" when unresolved (namespace not yet indexed, or genuinely
	// missing the label) — recordFor emits "-" rather than ever falling back to
	// pod.Namespace, which is a synthetic edge-local id, not the project (see
	// upstreamClusterNameLabel).
	project, projectOK := p.projectForNamespace(pod.Namespace)
	info := &podInfo{
		project:  project,
		instance: instance,
	}
	for _, c := range pod.Spec.Containers {
		info.vcpuMilli = max64(info.vcpuMilli, milliCPU(c.Resources.Limits[corev1.ResourceCPU]))
		if mem, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			info.memoryBytes = max64(info.memoryBytes, mem.Value())
		}
		info.vcpuMilli = max64(info.vcpuMilli, milliCPU(c.Resources.Requests[corev1.ResourceCPU]))
		if mem, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			info.memoryBytes = max64(info.memoryBytes, mem.Value())
		}
	}

	p.mu.Lock()
	prev, existed := p.pods[pod.Status.PodIP]
	p.pods[pod.Status.PodIP] = info
	total := len(p.pods)
	p.mu.Unlock()

	// The informer's 30s resync replays every pod in the cluster, so log only
	// when the indexed value actually changed — otherwise this floods.
	if !existed {
		p.stats.podIndexed.Add(1)
		p.debugf("podindex added ip=%s %s pod=%s/%s total=%d",
			pod.Status.PodIP, info, pod.Namespace, pod.Name, total)
	} else if !prev.equal(info) {
		p.debugf("podindex updated ip=%s %s (was %s) pod=%s/%s",
			pod.Status.PodIP, info, prev, pod.Namespace, pod.Name)
	}
	if instance == "" {
		p.debugf("podindex warn=missing_instance_label ip=%s pod=%s/%s label=%s",
			pod.Status.PodIP, pod.Namespace, pod.Name, upstreamInstanceLabel)
	} else if !projectOK {
		// A real provider Pod (it carries upstream.instance) whose namespace has
		// no upstream-cluster-name label. compute's own controller treats an
		// absent label here as misconfiguration, not a transient state — loud,
		// unconditional (not debug-gated), since this is a billing-correctness
		// signal, same tier as "resolve failed" elsewhere in this file.
		p.stats.projectLabelMissing.Add(1)
		log.Printf("podindex ALERT=missing_project_label ns=%s pod=%s/%s label=%s (record emitted with project=\"-\")",
			pod.Namespace, pod.Namespace, pod.Name, upstreamClusterNameLabel)
	}
}

func (p *projector) deletePod(pod *corev1.Pod) {
	if pod == nil || pod.Status.PodIP == "" {
		return
	}
	p.mu.Lock()
	_, existed := p.pods[pod.Status.PodIP]
	delete(p.pods, pod.Status.PodIP)
	total := len(p.pods)
	p.mu.Unlock()
	if existed {
		p.debugf("podindex removed ip=%s pod=%s/%s total=%d",
			pod.Status.PodIP, pod.Namespace, pod.Name, total)
	}
}

func milliCPU(q resource.Quantity) int64 {
	if q.IsZero() {
		return 0
	}
	return q.MilliValue()
}

// lookupIP returns the podInfo for a guest IP, plus whether it was found.
func (p *projector) lookupIP(ip string) (*podInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	info, ok := p.pods[ip]
	return info, ok
}

// listenAndServe accepts vm.state_change streams from ukpd and processes events.
func (p *projector) listenAndServe(ctx context.Context) error {
	// Remove a stale socket left by a prior run.
	if err := os.Remove(p.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", p.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.socketPath, err)
	}
	defer ln.Close()
	log.Printf("conn listening socket=%s", p.socketPath)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("conn accept_error err=%v", err)
			continue
		}
		id := p.stats.connections.Add(1)
		log.Printf("conn accepted id=%d", id)
		go func(c net.Conn, id int64) {
			defer c.Close()
			p.handleConnection(c, id)
		}(conn, id)
	}
}

func (p *projector) handleConnection(conn net.Conn, id int64) {
	start := time.Now()
	var events int64
	dec := json.NewDecoder(bufio.NewReader(conn))
	for {
		// Decode to raw bytes first so a payload we cannot interpret can be
		// logged exactly as ukpd sent it.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				log.Printf("conn closed id=%d events=%d duration=%s", id, events, time.Since(start).Round(time.Millisecond))
				return
			}
			p.stats.decodeErrors.Add(1)
			log.Printf("conn decode_error id=%d events=%d err=%v", id, events, err)
			return
		}
		var ev stateChange
		if err := json.Unmarshal(raw, &ev); err != nil {
			p.stats.decodeErrors.Add(1)
			log.Printf("conn unmarshal_error id=%d err=%v raw=%s", id, err, truncate(raw, 512))
			return
		}
		ev.Raw = raw
		events++
		p.stats.eventsReceived.Add(1)
		p.handleEvent(ev)
	}
}

func (p *projector) handleEvent(ev stateChange) {
	if ev.Type != "" && ev.Type != "vm.state_change" {
		p.stats.eventsWrongTyp.Add(1)
		p.debugf("event ignored reason=wrong_type type=%q", ev.Type)
		return
	}
	ts := parseTime(ev.Timestamp)
	if ts.IsZero() {
		ts = time.Now().UTC()
		p.debugf("event warn=unparseable_timestamp raw_timestamp=%q using=now", ev.Timestamp)
	}
	uuid, oldState, newState := extractTransition(ev.Data)
	if uuid == "" {
		// Logged loudly with the payload: if the vendor renames its uuid field
		// this is the only signal, and it silently stops all billing.
		p.stats.noUUID.Add(1)
		log.Printf("event dropped reason=no_uuid raw=%s", truncate(p.rawOf(ev), 512))
		return
	}
	// An unparseable new state tells us nothing about whether the instance is
	// consuming, so leave any open window alone rather than guessing.
	if newState == "" {
		p.stats.noState.Add(1)
		log.Printf("event dropped reason=no_new_state uuid=%s raw=%s", uuid, truncate(p.rawOf(ev), 512))
		return
	}
	enteredOn := newState == onState

	p.winMu.Lock()
	defer p.winMu.Unlock()

	w := p.windows[uuid]
	if enteredOn {
		if w == nil {
			w = &windowState{uuid: uuid, runningSince: ts, reportedUntil: ts}
			p.windows[uuid] = w
			p.stats.windowsOpened.Add(1)
			log.Printf("window opened uuid=%s prev=%q new=%q at=%s open_windows=%d",
				uuid, oldState, newState, ts.Format(time.RFC3339), len(p.windows))
			// A window anchored well behind wall clock (a replayed/buffered
			// event) will be flushed against time.Now(), so the first flush
			// bills the whole gap. Flag it here, before that record is written.
			if skew := time.Since(ts); skew > maxEventSkew {
				p.stats.staleEvents.Add(1)
				log.Printf("window WARN=stale_open uuid=%s event_time=%s skew=%s "+
					"(first flush will bill the gap to wall clock)",
					uuid, ts.Format(time.RFC3339), skew.Round(time.Second))
			}
			return
		}
		// Already running: no-op (started is idempotent).
		p.debugf("event noop reason=already_running uuid=%s prev=%q new=%q running_since=%s",
			uuid, oldState, newState, w.runningSince.Format(time.RFC3339))
		return
	}
	if w == nil {
		p.debugf("event noop reason=not_running uuid=%s prev=%q new=%q", uuid, oldState, newState)
		return
	}
	// Any non-running state closes the window. The open window — not the
	// event's old-state field — is the authority on whether we were running, so
	// a missing/renamed "prev" cannot strand a window open and silently stop
	// billing a stopped instance.
	p.emitWindow(w, ts, "close")
	delete(p.windows, uuid)
	p.stats.windowsClosed.Add(1)
	log.Printf("window closed uuid=%s prev=%q new=%q running_since=%s total_s=%.1f records=%d open_windows=%d",
		uuid, oldState, newState, w.runningSince.Format(time.RFC3339),
		ts.Sub(w.runningSince).Seconds(), w.records, len(p.windows))
}

// rawOf returns the exact wire bytes when available, falling back to the parsed
// data (tests and any path that did not come off the socket).
func (p *projector) rawOf(ev stateChange) []byte {
	if len(ev.Raw) > 0 {
		return ev.Raw
	}
	b, _ := json.Marshal(ev)
	return b
}

// periodicFlush incrementally reports open running windows so a long-running
// instance bills continuously and a crash loses at most one flush interval.
//
// Time bases: a window's start comes from the event's own timestamp, while the
// flush end comes from wall clock. These normally agree (ukpd shares this node's
// clock), but a replayed event anchors a window in the past and the next flush
// then bills start -> now in one record. The "stale_open" and "overbilled" logs
// mark both ends of that hazard.
func (p *projector) periodicFlush(ctx context.Context) {
	tick := time.NewTicker(p.flushInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now := time.Now().UTC()
			p.winMu.Lock()
			var flushed, skipped int
			for _, w := range p.windows {
				if now.Sub(w.reportedUntil) < p.flushInterval {
					skipped++
					continue
				}
				p.emitWindow(w, now, "flush")
				flushed++
			}
			open := len(p.windows)
			p.winMu.Unlock()
			if open > 0 {
				p.debugf("window flush_cycle open=%d flushed=%d skipped=%d", open, flushed, skipped)
			}
			p.cleanupOldRotations()
		}
	}
}

// emitWindow writes a windowed usage record covering [w.reportedUntil, end] and
// advances the reportedUntil watermark. Caller holds winMu. The cause ("close"
// or "flush") is carried into the log so a record can be traced to what
// produced it.
func (p *projector) emitWindow(w *windowState, end time.Time, cause string) {
	if end.Before(w.reportedUntil) {
		// The watermark is already past this event's time, which means a
		// wall-clock flush billed a span that event time says had not happened
		// yet — i.e. we have already over-billed by this much and the true close
		// is about to be clamped to nothing. Loud, because it is a money bug and
		// otherwise leaves no trace. See the "time bases" note on periodicFlush.
		log.Printf("window ALERT=overbilled uuid=%s cause=%s overbilled_s=%.1f event_end=%s watermark=%s",
			w.uuid, cause, w.reportedUntil.Sub(end).Seconds(),
			end.Format(time.RFC3339), w.reportedUntil.Format(time.RFC3339))
		p.stats.overbilled.Add(1)
		end = w.reportedUntil
	}
	if end.Equal(w.reportedUntil) {
		p.debugf("window skip reason=zero_duration uuid=%s cause=%s at=%s",
			w.uuid, cause, end.Format(time.RFC3339))
		return
	}
	rec := p.recordFor(w, end)
	if err := appendRecord(p.outputPath, rec); err != nil {
		// The window is not advanced, so the next flush retries this span
		// rather than losing it.
		p.stats.writeErrors.Add(1)
		log.Printf("record write_error uuid=%s cause=%s path=%s err=%v", w.uuid, cause, p.outputPath, err)
		return
	}
	w.reportedUntil = end
	w.records++
	p.stats.recordsWritten.Add(1)
	// The emitted record is the billing artifact, so log it in full: this is
	// the line to compare against what Vector shipped.
	log.Printf("record written cause=%s id=%s uuid=%s project=%s instance=%s vcpu=%d memory_bytes=%d start=%s end=%s duration_s=%.1f",
		cause, rec.ID, rec.UUID, rec.Project, rec.Instance, rec.VCPU, rec.MemoryBytes, rec.Start, rec.End, rec.DurationS)
	p.rotateIfNeeded()
}

// rotateIfNeeded renames the output file once it reaches rotateSizeBytes, so
// the live file a reader has open keeps growing under its original name while
// a rotated-away one stops changing and gets a final EOF. The next
// appendRecord recreates outputPath fresh (O_CREATE). Renaming — never
// truncating in place — is what makes this safe for something already
// reading the file: a rename only changes a directory entry, so an existing
// open handle keeps reading the same underlying data, undisturbed, to
// whatever became its final content.
func (p *projector) rotateIfNeeded() {
	if p.rotateSizeBytes <= 0 {
		return
	}
	info, err := os.Stat(p.outputPath)
	if err != nil {
		return // nothing to rotate yet
	}
	if info.Size() < p.rotateSizeBytes {
		return
	}
	rotated := fmt.Sprintf("%s.%d", p.outputPath, time.Now().Unix())
	if err := os.Rename(p.outputPath, rotated); err != nil {
		log.Printf("rotate error=%v path=%s size_bytes=%d", err, p.outputPath, info.Size())
		return
	}
	p.stats.rotations.Add(1)
	log.Printf("rotate done from=%s to=%s size_bytes=%d threshold_bytes=%d",
		p.outputPath, rotated, info.Size(), p.rotateSizeBytes)
}

// cleanupOldRotations deletes rotated files older than rotateMaxAge. This is
// a disk-safety backstop, not the primary cleanup path — the correct owner of
// deletion is Vector, which alone knows a file was fully shipped downstream;
// this only exists for the gap before Vector is wired up, or if it is ever
// down longer than rotateMaxAge. Age is read from the rotation timestamp
// embedded in the filename by rotateIfNeeded, not the file's mtime, so it
// reflects "time since rotated" exactly rather than "time since last write"
// (which rename does not update anyway).
func (p *projector) cleanupOldRotations() {
	if p.rotateMaxAge <= 0 {
		return
	}
	matches, err := filepath.Glob(p.outputPath + ".*")
	if err != nil {
		log.Printf("rotate cleanup_error err=%v", err)
		return
	}
	now := time.Now()
	for _, f := range matches {
		idx := strings.LastIndex(f, ".")
		if idx < 0 {
			continue
		}
		ts, err := strconv.ParseInt(f[idx+1:], 10, 64)
		if err != nil {
			// Not one of ours (or malformed) — skip rather than guess at
			// deleting a file we didn't create.
			continue
		}
		age := now.Sub(time.Unix(ts, 0))
		if age < p.rotateMaxAge {
			continue
		}
		if err := os.Remove(f); err != nil {
			log.Printf("rotate delete_error path=%s age=%s err=%v", f, age.Round(time.Second), err)
			continue
		}
		p.stats.rotationDeletes.Add(1)
		log.Printf("rotate deleted reason=age_backstop path=%s age=%s max_age=%s",
			f, age.Round(time.Second), p.rotateMaxAge)
	}
}

// recordFor resolves the instance's identity/resources and builds the record.
func (p *projector) recordFor(w *windowState, end time.Time) usageRecord {
	info := w.resolved
	if info == nil {
		var reason string
		info, reason = p.resolve(w.uuid)
		w.resolved = info
		switch {
		case info != nil:
			log.Printf("resolve ok uuid=%s %s", w.uuid, info)
		case reason != w.lastResolveLog:
			// Attribution retries every flush; log each distinct reason once per
			// window so a persistent failure does not repeat indefinitely.
			p.stats.unresolved.Add(1)
			log.Printf("resolve failed uuid=%s reason=%s platform_dir=%s (record emitted with project=\"-\")",
				w.uuid, reason, p.platformDir)
			w.lastResolveLog = reason
		}
	}
	start := w.reportedUntil
	dur := end.Sub(start).Seconds()
	if dur < 0 {
		dur = 0
	}
	id := windowID(w.uuid, start, end)
	rec := usageRecord{
		ID:          id,
		Project:     "-",
		Instance:    "-",
		UUID:        w.uuid,
		VCPU:        0,
		MemoryBytes: 0,
		Start:       start.UTC().Format(time.RFC3339),
		End:         end.UTC().Format(time.RFC3339),
		DurationS:   dur,
	}
	if info != nil {
		// An empty project (namespace unresolved or genuinely unlabeled) stays
		// "-" rather than becoming an empty string in the record — same
		// fail-loud convention as the fully-unresolved case above.
		if info.project != "" {
			rec.Project = info.project
		}
		rec.Instance = info.instance
		rec.VCPU = coresFromMilli(info.vcpuMilli)
		rec.MemoryBytes = info.memoryBytes
	}
	return rec
}

// resolve maps an instance uuid to identity/resources by reading the guest IP from
// its vmm.json and looking that IP up in the pod index. The returned reason
// names which stage failed, since each has a different fix.
func (p *projector) resolve(uuid string) (*podInfo, string) {
	ip, err := guestIP(p.platformDir, uuid)
	if err != nil {
		if errors.Is(err, errNoNetdevIP) {
			return nil, reasonNoNetdevIP
		}
		p.debugf("resolve detail uuid=%s stage=vmm_json err=%v", uuid, err)
		return nil, reasonVMMUnreadble
	}
	info, ok := p.lookupIP(ip)
	if !ok {
		p.mu.RLock()
		indexed := len(p.pods)
		p.mu.RUnlock()
		p.debugf("resolve detail uuid=%s stage=pod_index guest_ip=%s indexed_pod_ips=%d", uuid, ip, indexed)
		return nil, reasonPodNotIndexed
	}
	p.debugf("resolve detail uuid=%s guest_ip=%s %s", uuid, ip, info)
	return info, reasonOK
}

// logStats emits the cumulative counter heartbeat. It runs unconditionally (not
// only under -debug) and is the first line to read when diagnosing: if
// events_received stays 0 the sink never connected, and if records_written
// stays 0 while windows open, attribution or the output path is at fault.
func (p *projector) logStats(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.mu.RLock()
			indexed := len(p.pods)
			p.mu.RUnlock()
			p.winMu.Lock()
			open := len(p.windows)
			p.winMu.Unlock()

			log.Printf("stats uptime=%s conns=%d events_received=%d events_wrong_type=%d "+
				"dropped_no_uuid=%d dropped_no_state=%d decode_errors=%d "+
				"windows_open=%d windows_opened=%d windows_closed=%d "+
				"records_written=%d write_errors=%d unresolved=%d indexed_pod_ips=%d watch_errors=%d "+
				"stale_events=%d overbilled=%d rotations=%d rotation_deletes=%d project_label_missing=%d",
				time.Since(start).Round(time.Second),
				p.stats.connections.Load(), p.stats.eventsReceived.Load(), p.stats.eventsWrongTyp.Load(),
				p.stats.noUUID.Load(), p.stats.noState.Load(), p.stats.decodeErrors.Load(),
				open, p.stats.windowsOpened.Load(), p.stats.windowsClosed.Load(),
				p.stats.recordsWritten.Load(), p.stats.writeErrors.Load(),
				p.stats.unresolved.Load(), indexed, p.stats.watchErrors.Load(),
				p.stats.staleEvents.Load(), p.stats.overbilled.Load(),
				p.stats.rotations.Load(), p.stats.rotationDeletes.Load(),
				p.stats.projectLabelMissing.Load())
		}
	}
}

func coresFromMilli(milli int64) int64 {
	return milli / 1000
}

// errNoNetdevIP distinguishes "vmm.json exists but has no guest IP" from "could
// not read vmm.json", which have different causes.
var errNoNetdevIP = errors.New("netdev.ip not found in vmm.json")

// guestIP extracts the guest IP for an instance from its vmm.json workspace file.
func guestIP(platformDir, uuid string) (string, error) {
	path := filepath.Join(platformDir, uuid, "vmm.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	m := netdevIPRe.FindSubmatch(b)
	if len(m) < 2 {
		return "", fmt.Errorf("%s: %w", path, errNoNetdevIP)
	}
	return string(m[1]), nil
}

// windowID is a deterministic id for a (uuid, start, end) window, for downstream
// dedup on replay.
func windowID(uuid string, start, end time.Time) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%s|%s|%s", uuid, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))))
	return hex.EncodeToString(sum[:])
}

func appendRecord(path string, rec usageRecord) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// extractTransition pulls uuid + old/new state out of the event data object,
// tolerating the vendor's varying key conventions.
func extractTransition(data map[string]interface{}) (uuid, oldState, newState string) {
	if data == nil {
		return "", "", ""
	}
	// "vm"/"new"/"prev" are what ukpd actually emits; the remaining spellings are
	// tolerated for other vendor builds. Keep the real keys first so a payload
	// carrying both cannot be read off the wrong field.
	uuid = firstString(data, "vm", "uuid", "id", "instance_id", "instance_uuid")
	if uuid == "" {
		// Fall back to scanning the whole data blob for a uuid-shaped token.
		raw, _ := json.Marshal(data)
		if m := uuidRe.Find(raw); m != nil {
			uuid = string(m)
		}
	}
	newState = firstString(data, "new", "new_state", "state", "to", "target_state", "state_to")
	oldState = firstString(data, "prev", "old_state", "previous_state", "from", "state_from")
	return uuid, oldState, newState
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// parseTime accepts RFC3339 or Unix-epoch seconds (string or number).
func parseTime(v interface{}) time.Time {
	switch t := v.(type) {
	case string:
		if tt, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return tt
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return time.Unix(n, 0).UTC()
		}
	case float64:
		return time.Unix(int64(t), 0).UTC()
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}

// truncate bounds a payload logged verbatim so a pathological event cannot
// flood the log.
func truncate(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + fmt.Sprintf("...(%d bytes truncated)", len(b)-limit)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
