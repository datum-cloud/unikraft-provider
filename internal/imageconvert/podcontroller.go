package imageconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	// Gate is the scheduling gate the provider creates Pods with.
	Gate = "compute.datumapis.com/image-conversion"

	// AnnotationClaim records the adapter working on a gated Pod and since
	// when, as "<node>@<unix seconds>". It is removed on release.
	AnnotationClaim = "compute.datumapis.com/image-conversion-claim"
	// AnnotationSources maps each converted image's layout digest to its
	// Source, as JSON, so any node can rebuild it.
	AnnotationSources = "compute.datumapis.com/image-conversion-sources"

	// ConditionImagesReady reports a gated Pod's conversion: False while
	// converting, or with why it failed, in which case it is retried with
	// backoff; True once the Pod is released. Failure reasons classify the
	// failure, and the message holds the detail for operators.
	ConditionImagesReady      core.PodConditionType = "compute.datumapis.com/ImagesReady"
	ReasonConverting                                = "Converting"
	ReasonPullFailed                                = "ImagePullFailed"
	ReasonUnsupportedPlatform                       = "UnsupportedPlatform"
	ReasonFailed                                    = "ConversionFailed"
	ReasonConverted                                 = "Converted"

	imageScheme = "http+oci://"
)

// PodReconciler converts the images of gated Pods. Any node's adapter may
// claim a Pod; it converts each container's image, or keeps one that already
// runs on the runtime, then points the Pod at them and removes the gate in one
// update. Until then the Pod's images are the provider's, so an adapter that
// restarts mid-way starts over from them.
type PodReconciler struct {
	// Client reads Pods from the cache and updates them.
	Client client.Client
	// Secrets reads pull Secrets uncached.
	Secrets   client.Reader
	Converter *Converter
	// Node is this adapter's node, recorded in claims.
	Node string
	// AdvertiseURL is the http+oci URL the runtime's image agent reaches
	// adapters at, the same on every node. Converted images are named
	// under it.
	AdvertiseURL string
	// ClaimTTL is how long a claim keeps other adapters off a Pod.
	ClaimTTL time.Duration
	// PreferredNode, if set, is the node released Pods prefer: the one whose
	// runtime this adapter serves, which already holds their images.
	PreferredNode string
}

// SetupWithManager registers the reconciler for gated Pods.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("image-conversion").
		For(&core.Pod{}, builder.WithPredicates(
			predicate.NewPredicateFuncs(func(o client.Object) bool { return gated(o.(*core.Pod)) }),
			// Its own condition writes would otherwise retry a failed Pod at
			// once, past the backoff.
			predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
				return !statusOnly(e.ObjectOld.(*core.Pod), e.ObjectNew.(*core.Pod))
			}},
		)).
		// Conversions are capped by the Converter; the rest of a reconcile is
		// quick, so Pods whose images are ready don't wait behind them.
		WithOptions(controller.Options{MaxConcurrentReconciles: 16}).
		Complete(r)
}

func gated(pod *core.Pod) bool {
	return slices.ContainsFunc(pod.Spec.SchedulingGates, isGate)
}

func isGate(g core.PodSchedulingGate) bool { return g.Name == Gate }

func isImagesReady(c core.PodCondition) bool { return c.Type == ConditionImagesReady }

func statusOnly(old, pod *core.Pod) bool {
	return equality.Semantic.DeepEqual(old.Spec, pod.Spec) &&
		maps.Equal(old.Annotations, pod.Annotations) &&
		old.DeletionTimestamp.Equal(pod.DeletionTimestamp)
}

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod core.Pod
	if err := r.Client.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !gated(&pod) || !pod.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if wait := r.claimedElsewhere(&pod); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	if err := r.claim(ctx, &pod); err != nil {
		return requeueOnConflict(err)
	}
	// Retries keep reporting the last failure until one succeeds.
	if !slices.ContainsFunc(pod.Status.Conditions, isImagesReady) {
		if err := r.setImagesReady(ctx, &pod, core.ConditionFalse, ReasonConverting, "Converting the Pod's images"); err != nil {
			return requeueOnConflict(err)
		}
	}

	images, sources, err := r.convert(ctx, &pod)
	if err != nil {
		if serr := r.setImagesReady(ctx, &pod, core.ConditionFalse, failureReason(err), err.Error()); serr != nil {
			ctrl.LoggerFrom(ctx).Error(serr, "recording the conversion error")
		}
		return ctrl.Result{}, err
	}
	if err := r.setImagesReady(ctx, &pod, core.ConditionTrue, ReasonConverted, ""); err != nil {
		return requeueOnConflict(err)
	}

	for i, c := range pod.Spec.Containers {
		pod.Spec.Containers[i].Image = images[c.Name]
	}
	if len(sources) > 0 {
		b, err := json.Marshal(sources)
		if err != nil {
			return ctrl.Result{}, err
		}
		pod.Annotations[AnnotationSources] = string(b)
	}
	r.preferNode(&pod)
	pod.Spec.SchedulingGates = slices.DeleteFunc(pod.Spec.SchedulingGates, isGate)
	delete(pod.Annotations, AnnotationClaim)
	if err := r.Client.Update(ctx, &pod); err != nil {
		return requeueOnConflict(err)
	}
	ctrl.LoggerFrom(ctx).Info("released pod", "images", images)
	return ctrl.Result{}, nil
}

// claimedElsewhere returns how long another adapter's claim stays fresh.
func (r *PodReconciler) claimedElsewhere(pod *core.Pod) time.Duration {
	node, ts, ok := strings.Cut(pod.Annotations[AnnotationClaim], "@")
	secs, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil || node == r.Node {
		return 0
	}
	return max(time.Until(time.Unix(secs, 0).Add(r.ClaimTTL)), 0)
}

// claim records this adapter's claim. Concurrent claims conflict on the
// Pod's resource version, so one wins.
func (r *PodReconciler) claim(ctx context.Context, pod *core.Pod) error {
	if node, _, _ := strings.Cut(pod.Annotations[AnnotationClaim], "@"); node == r.Node {
		return nil
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationClaim] = r.Node + "@" + strconv.FormatInt(time.Now().Unix(), 10)
	return r.Client.Update(ctx, pod)
}

// preferNode makes the Pod prefer PreferredNode, so it usually starts where its
// images were converted rather than rebuilding them elsewhere. It is only a
// preference, so the Pod still runs on any other node that fits.
func (r *PodReconciler) preferNode(pod *core.Pod) {
	if r.PreferredNode == "" {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &core.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &core.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	na.PreferredDuringSchedulingIgnoredDuringExecution = append(na.PreferredDuringSchedulingIgnoredDuringExecution, core.PreferredSchedulingTerm{
		Weight: 100,
		Preference: core.NodeSelectorTerm{MatchFields: []core.NodeSelectorRequirement{{
			Key:      "metadata.name",
			Operator: core.NodeSelectorOpIn,
			Values:   []string{r.PreferredNode},
		}}},
	})
}

func failureReason(err error) string {
	switch {
	case errors.Is(err, errUnauthorized):
		return ReasonPullFailed
	case errors.Is(err, errUnsupportedPlatform):
		return ReasonUnsupportedPlatform
	}
	return ReasonFailed
}

// setImagesReady sets the Pod's ImagesReady condition unless it already reads
// so. The patch holds only that condition, which Pod status merges by type, so
// it leaves the others alone.
func (r *PodReconciler) setImagesReady(ctx context.Context, pod *core.Pod, status core.ConditionStatus, reason, message string) error {
	cond := core.PodCondition{
		Type:               ConditionImagesReady,
		Status:             status,
		Reason:             reason,
		Message:            truncate(message, 1024),
		LastTransitionTime: metav1.Now(),
	}
	if i := slices.IndexFunc(pod.Status.Conditions, isImagesReady); i >= 0 {
		old := pod.Status.Conditions[i]
		if old.Status == cond.Status && old.Reason == cond.Reason && old.Message == cond.Message {
			return nil
		}
		if old.Status == cond.Status {
			cond.LastTransitionTime = old.LastTransitionTime
		}
	}
	patch, err := json.Marshal(map[string]any{"status": map[string]any{"conditions": []core.PodCondition{cond}}})
	if err != nil {
		return err
	}
	// A copy: the patch returns the server's Pod, whose newer resource version
	// would let a stale reconcile's update through.
	return r.Client.Status().Patch(ctx, pod.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, patch))
}

// convert returns each container's image as the Pod should refer to it, and
// the sources of those converted.
func (r *PodReconciler) convert(ctx context.Context, pod *core.Pod) (map[string]string, map[digest.Digest]Source, error) {
	keys, err := keyringFor(ctx, r.Secrets, pod)
	if err != nil {
		return nil, nil, err
	}
	images := map[string]string{}
	sources := map[digest.Digest]Source{}
	for _, c := range pod.Spec.Containers {
		// Only adapters name converted images, for Pods they checked.
		if strings.HasPrefix(c.Image, imageScheme) {
			return nil, nil, fmt.Errorf("container %q: %s images cannot be used directly", c.Name, imageScheme)
		}
		prepared, err := r.Converter.Prepare(ctx, c.Image, keys)
		if err != nil {
			return nil, nil, fmt.Errorf("container %q: %w", c.Name, err)
		}
		images[c.Name] = c.Image
		if !prepared.Preconverted {
			images[c.Name] = r.AdvertiseURL + prepared.Path
			sources[prepared.Converted.Layout] = prepared.Converted.Source
		}
	}
	return images, sources, nil
}

// sourcesOf returns the sources of a Pod's converted images.
func sourcesOf(pod *core.Pod) map[digest.Digest]Source {
	var sources map[digest.Digest]Source
	_ = json.Unmarshal([]byte(pod.Annotations[AnnotationSources]), &sources)
	return sources
}

// keyringFor reads a Pod's pull Secrets.
func keyringFor(ctx context.Context, secrets client.Reader, pod *core.Pod) (*Keyring, error) {
	keys := &Keyring{}
	for _, ref := range pod.Spec.ImagePullSecrets {
		var secret core.Secret
		if err := secrets.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &secret); err != nil {
			return nil, fmt.Errorf("reading pull secret %q: %w", ref.Name, err)
		}
		if err := keys.AddSecret(&secret); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// requeueOnConflict retries a conflicting update and drops one for a Pod
// deleted meanwhile.
func requeueOnConflict(err error) (ctrl.Result, error) {
	switch {
	case apierrors.IsConflict(err):
		return ctrl.Result{Requeue: true}, nil
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
