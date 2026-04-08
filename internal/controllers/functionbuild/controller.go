package functionbuild

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

const (
	kraftImageDefault         = "ghcr.io/unikraft/kraftkit:latest"
	kraftCloudTokenSecretName = "kraftcloud-credentials"
	kraftCloudTokenSecretKey  = "token"
	kraftCloudSecretNamespace = "datum-system"
)

// buildResult is the JSON structure written by the build job to the output ConfigMap.
type buildResult struct {
	ImageRef string `json:"imageRef"`
}

// Reconciler reconciles FunctionBuild resources.
type Reconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	KraftImage string
}

// SetupWithManager registers the controller with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.KraftImage == "" {
		r.KraftImage = kraftImageDefault
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&functionsv1alpha1.FunctionBuild{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

// Reconcile processes a FunctionBuild resource.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var build functionsv1alpha1.FunctionBuild
	if err := r.Get(ctx, req.NamespacedName, &build); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get function build: %w", err)
	}

	if !build.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	log.Info("reconciling function build")

	// If already in a terminal state, nothing more to do.
	if build.Status.Phase == functionsv1alpha1.BuildPhaseSucceeded ||
		build.Status.Phase == functionsv1alpha1.BuildPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Ensure the Job exists.
	if err := r.reconcileJob(ctx, &build); err != nil {
		return ctrl.Result{}, err
	}

	// Sync status from the Job.
	return ctrl.Result{}, r.syncStatus(ctx, &build)
}

// reconcileJob creates the build Job if it does not exist.
func (r *Reconciler) reconcileJob(ctx context.Context, build *functionsv1alpha1.FunctionBuild) error {
	jobName := fmt.Sprintf("build-%s", build.Name)

	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: build.Namespace}, &job)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get build job: %w", err)
	}

	gitURL := ""
	gitBranch := "main"
	if build.Spec.Source.Git != nil {
		gitURL = build.Spec.Source.Git.URL
		if build.Spec.Source.Git.Ref.Commit != "" {
			gitBranch = build.Spec.Source.Git.Ref.Commit
		} else if build.Spec.Source.Git.Ref.Tag != "" {
			gitBranch = build.Spec.Source.Git.Ref.Tag
		} else if build.Spec.Source.Git.Ref.Branch != "" {
			gitBranch = build.Spec.Source.Git.Ref.Branch
		}
	}

	resultConfigMapName := fmt.Sprintf("build-result-%s", jobName)
	backoffLimit := int32(3)

	job = batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: build.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{
						{
							Name:       "git-clone",
							Image:      "alpine/git:latest",
							Command:    []string{"git", "clone", gitURL, "--branch", gitBranch, "--depth", "1", "/workspace"},
							WorkingDir: "/",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:       "kraft-build",
							Image:      r.KraftImage,
							Command:    []string{"kraft", "cloud", "build", "--output", "json", "."},
							WorkingDir: "/workspace",
							Env: []corev1.EnvVar{
								{
									Name: "KRAFTCLOUD_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: kraftCloudTokenSecretName,
											},
											Key: kraftCloudTokenSecretKey,
										},
									},
								},
								{
									Name:  "BUILD_RESULT_CONFIGMAP",
									Value: resultConfigMapName,
								},
								{
									Name:  "BUILD_RESULT_NAMESPACE",
									Value: build.Namespace,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(build, &job, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on job: %w", err)
	}

	if err := r.Create(ctx, &job); err != nil {
		return fmt.Errorf("failed to create build job: %w", err)
	}

	ctrl.LoggerFrom(ctx).Info("created build job", "job", jobName)

	// Set initial Running status.
	now := metav1.Now()
	patch := client.MergeFrom(build.DeepCopy())
	build.Status.Phase = functionsv1alpha1.BuildPhaseRunning
	build.Status.JobRef = jobName
	build.Status.StartTime = &now
	if err := r.Status().Patch(ctx, build, patch); err != nil {
		return fmt.Errorf("failed to patch build status to running: %w", err)
	}
	return nil
}

// syncStatus reads the Job state and updates the FunctionBuild status accordingly.
func (r *Reconciler) syncStatus(ctx context.Context, build *functionsv1alpha1.FunctionBuild) error {
	jobName := fmt.Sprintf("build-%s", build.Name)

	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: build.Namespace}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get build job for status sync: %w", err)
	}

	// Check for failure.
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			now := metav1.Now()
			patch := client.MergeFrom(build.DeepCopy())
			build.Status.Phase = functionsv1alpha1.BuildPhaseFailed
			build.Status.CompletionTime = &now
			build.Status.Message = cond.Message
			return r.Status().Patch(ctx, build, patch)
		}
	}

	// Check for success.
	if job.Status.Succeeded > 0 {
		resultConfigMapName := fmt.Sprintf("build-result-%s", jobName)
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: resultConfigMapName, Namespace: build.Namespace}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				// ConfigMap not yet written.
				return nil
			}
			return fmt.Errorf("failed to get build result configmap: %w", err)
		}

		resultData, ok := cm.Data["result"]
		if !ok {
			return fmt.Errorf("build result configmap %s does not contain 'result' key", resultConfigMapName)
		}

		var result buildResult
		if err := json.Unmarshal([]byte(resultData), &result); err != nil {
			return fmt.Errorf("failed to parse build result from configmap: %w", err)
		}

		now := metav1.Now()
		patch := client.MergeFrom(build.DeepCopy())
		build.Status.Phase = functionsv1alpha1.BuildPhaseSucceeded
		build.Status.ImageRef = result.ImageRef
		build.Status.CompletionTime = &now
		build.Status.Message = "Build completed successfully"
		return r.Status().Patch(ctx, build, patch)
	}

	return nil
}
