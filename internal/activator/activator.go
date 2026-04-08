// Package activator implements hold-and-proxy semantics for scale-to-zero
// Functions. When a Function has no running replicas the HTTPProxy routes
// traffic here. The activator buffers inbound requests, signals scale-up, and
// forwards each request once the Workload reports at least one ready replica.
package activator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

const (
	// maxQueueDepth is the maximum number of pending requests per function.
	maxQueueDepth = 100

	// scaleUpTimeout is how long the activator will wait for a cold-start before
	// returning 504 to all buffered requests.
	scaleUpTimeout = 10 * time.Second

	// pollInterval is the frequency at which the activator polls Workload readiness.
	pollInterval = 250 * time.Millisecond

	// annotationScaleFromZero is patched onto a Function to signal the
	// FunctionRevisionController that it should set minReplicas = 1 on the Workload.
	annotationScaleFromZero = "functions.datumapis.com/scale-from-zero"
)

// pendingRequest is a single buffered HTTP transaction.
type pendingRequest struct {
	w    http.ResponseWriter
	r    *http.Request
	// done is closed by the activator once the request has been forwarded (or
	// failed). The handler goroutine waits on done before returning.
	done chan struct{}
}

// funcQueue is the per-function buffer and scale-up state.
type funcQueue struct {
	ch     chan pendingRequest
	// ready is closed once the Workload has at least one available replica.
	ready  chan struct{}
	// cancel stops the scale-up goroutine when no longer needed.
	cancel context.CancelFunc
}

// Activator is the main controller for hold-and-proxy behaviour.
type Activator struct {
	client     client.Client
	cache      cache.Cache
	index      *functionIndex
	httpClient *http.Client

	mu     sync.Mutex
	queues map[string]*funcQueue // keyed by "<namespace>/<name>"
}

// New creates an Activator. Start must be called before ServeHTTP.
func New(c client.Client, ca cache.Cache) *Activator {
	return &Activator{
		client:     c,
		cache:      ca,
		index:      newFunctionIndex(),
		httpClient: &http.Client{Timeout: 35 * time.Second},
		queues:     make(map[string]*funcQueue),
	}
}

// Start initialises the function index from the cache and registers event
// handlers so the index stays current. It blocks until ctx is cancelled.
func (a *Activator) Start(ctx context.Context) error {
	// Wait for the cache to sync so our initial list is complete.
	if !a.cache.WaitForCacheSync(ctx) {
		return fmt.Errorf("activator: cache sync timed out")
	}

	if err := a.index.syncFromCache(ctx, a.cache); err != nil {
		return fmt.Errorf("activator: initial index sync: %w", err)
	}

	// Watch Function resources and keep the index updated.
	fnInformer, err := a.cache.GetInformer(ctx, &functionsv1alpha1.Function{})
	if err != nil {
		return fmt.Errorf("activator: get function informer: %w", err)
	}

	fnInformer.AddEventHandler(functionEventHandler{index: a.index})

	<-ctx.Done()
	return nil
}

// handleRequest is the core request path. It is called from handler.go after
// the function identity has been resolved.
//
// It returns false only when the request cannot be accepted (queue full), in
// which case the caller must write the 503 response. On all other paths the
// function takes ownership of w and r.
func (a *Activator) handleRequest(ctx context.Context, nn types.NamespacedName, w http.ResponseWriter, r *http.Request) {
	key := nn.String()

	a.mu.Lock()
	q, exists := a.queues[key]
	if !exists {
		qctx, cancel := context.WithTimeout(context.Background(), scaleUpTimeout)
		q = &funcQueue{
			ch:     make(chan pendingRequest, maxQueueDepth),
			ready:  make(chan struct{}),
			cancel: cancel,
		}
		a.queues[key] = q
		// First request for this cold function: kick off scale-up.
		go a.scaleUp(qctx, nn, q)
	}
	a.mu.Unlock()

	pr := pendingRequest{w: w, r: r, done: make(chan struct{})}

	select {
	case q.ch <- pr:
		requestQueueDepth.WithLabelValues(nn.Namespace, nn.Name).Inc()
	default:
		// Queue is full.
		queueFullTotal.WithLabelValues(nn.Namespace, nn.Name).Inc()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"queue full"}`))
		return
	}

	// Wait until the activator has forwarded our request or the caller's context
	// (10 s request deadline set in handler.go) expires.
	select {
	case <-pr.done:
		// The activator forwarded (or failed) the request; response already written.
	case <-ctx.Done():
		// Caller's context expired while we were still waiting.
		timeoutTotal.WithLabelValues(nn.Namespace, nn.Name).Inc()
		w.WriteHeader(http.StatusGatewayTimeout)
	}
}

// scaleUp patches the Workload to minReplicas=1, then polls until available,
// then drains the queue by forwarding each request.
func (a *Activator) scaleUp(ctx context.Context, nn types.NamespacedName, q *funcQueue) {
	defer func() {
		q.cancel()
		close(q.ready)
		// Remove the queue entry so the next cold-start creates a fresh one.
		a.mu.Lock()
		delete(a.queues, nn.String())
		a.mu.Unlock()
	}()

	startTime := time.Now()

	endpoint, err := a.waitForReady(ctx, nn)
	if err != nil {
		slog.Error("scale-up failed", "function", nn, "err", err)
		// Drain queue with 504.
		a.drainWithError(q, http.StatusGatewayTimeout, `{"error":"cold start timeout"}`)
		return
	}

	elapsed := time.Since(startTime)
	slog.Info("function ready after cold start", "function", nn, "elapsed", elapsed)

	// Drain queue by forwarding each buffered request.
	close(q.ch) // signal no more enqueues; we own the drain from here
	for pr := range q.ch {
		requestQueueDepth.WithLabelValues(nn.Namespace, nn.Name).Dec()
		go func(pr pendingRequest) {
			defer close(pr.done)
			proxyRequest(pr.w, pr.r, endpoint)
		}(pr)
	}

	coldStartDuration.WithLabelValues(nn.Namespace, nn.Name).Observe(elapsed.Seconds())
}

// waitForReady patches the Workload to minReplicas=1 and polls until the
// Workload's Available condition is true. It returns the upstream endpoint URL.
func (a *Activator) waitForReady(ctx context.Context, nn types.NamespacedName) (string, error) {
	// Look up the Function to find its active revision.
	var fn functionsv1alpha1.Function
	if err := a.client.Get(ctx, nn, &fn); err != nil {
		return "", fmt.Errorf("get function: %w", err)
	}

	if err := a.signalScaleUp(ctx, &fn); err != nil {
		return "", fmt.Errorf("signal scale-up: %w", err)
	}

	// Poll until the Workload is available or the context expires.
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for workload to become ready")
		case <-ticker.C:
			endpoint, ready, err := a.checkWorkloadReady(ctx, &fn)
			if err != nil {
				slog.Warn("workload readiness check failed", "function", nn, "err", err)
				continue
			}
			if ready {
				return endpoint, nil
			}
		}
	}
}

// signalScaleUp patches the Function annotation to request scale-up.
// The FunctionRevisionController reacts to this annotation by setting
// Workload.spec.placements[0].scaleSettings.minReplicas = 1.
func (a *Activator) signalScaleUp(ctx context.Context, fn *functionsv1alpha1.Function) error {
	patch := client.MergeFrom(fn.DeepCopy())
	if fn.Annotations == nil {
		fn.Annotations = make(map[string]string)
	}
	fn.Annotations[annotationScaleFromZero] = metav1.Now().UTC().Format(time.RFC3339)
	if err := a.client.Patch(ctx, fn, patch); err != nil {
		return fmt.Errorf("patch function annotation: %w", err)
	}
	return nil
}

// checkWorkloadReady returns the endpoint URL and true when the Workload
// backing the active revision reports Available.
func (a *Activator) checkWorkloadReady(ctx context.Context, fn *functionsv1alpha1.Function) (string, bool, error) {
	if fn.Status.ActiveRevision == "" {
		// Re-fetch in case status was updated.
		if err := a.client.Get(ctx, types.NamespacedName{Namespace: fn.Namespace, Name: fn.Name}, fn); err != nil {
			return "", false, fmt.Errorf("re-fetch function: %w", err)
		}
		if fn.Status.ActiveRevision == "" {
			return "", false, nil
		}
	}

	var rev functionsv1alpha1.FunctionRevision
	if err := a.client.Get(ctx, types.NamespacedName{
		Namespace: fn.Namespace,
		Name:      fn.Status.ActiveRevision,
	}, &rev); err != nil {
		return "", false, fmt.Errorf("get revision: %w", err)
	}

	if rev.Status.WorkloadRef == "" {
		return "", false, nil
	}

	var workload computev1alpha.Workload
	if err := a.client.Get(ctx, types.NamespacedName{
		Namespace: fn.Namespace,
		Name:      rev.Status.WorkloadRef,
	}, &workload); err != nil {
		return "", false, fmt.Errorf("get workload: %w", err)
	}

	cond := apimeta.FindStatusCondition(workload.Status.Conditions, computev1alpha.WorkloadAvailable)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return "", false, nil
	}

	port := int32(8080)
	if rev.Spec.FunctionSpec.Runtime.Port != nil {
		port = *rev.Spec.FunctionSpec.Runtime.Port
	}
	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", rev.Status.WorkloadRef, fn.Namespace, port)
	return endpoint, true, nil
}

// drainWithError sends an error response to all buffered requests and marks
// them done.
func (a *Activator) drainWithError(q *funcQueue, statusCode int, body string) {
	close(q.ch)
	for pr := range q.ch {
		func(pr pendingRequest) {
			defer close(pr.done)
			pr.w.Header().Set("Content-Type", "application/json")
			pr.w.WriteHeader(statusCode)
			_, _ = pr.w.Write([]byte(body))
		}(pr)
	}
}
