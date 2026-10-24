package controller

import (
	"fmt"
	"strings"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	core "k8s.io/api/core/v1"
)

// With image conversion enabled, Instance Pods are created with a scheduling
// gate, which the OCI adapter (cmd/ukp-oci-adapter) removes once it has
// converted their images. These names match the adapter's
// (internal/imageconvert), which is not imported to keep its dependencies out
// of the provider.
const (
	imageConversionGate = "compute.datumapis.com/image-conversion"
	// imagesReadyCondition is False on a gated Pod while its images are being
	// converted, or with a reason classifying why they can't be.
	imagesReadyCondition     core.PodConditionType = "compute.datumapis.com/ImagesReady"
	imageConverting                                = "Converting"
	imagePullFailed                                = "ImagePullFailed"
	imageUnsupportedPlatform                       = "UnsupportedPlatform"

	// imageFormatAnnotation set to imageFormatUnikraft on an Instance skips
	// the gate: its images already run on the runtime.
	imageFormatAnnotation = "compute.datumapis.com/image-format"
	imageFormatUnikraft   = "unikraft"
)

// gatesForImageConversion reports whether an Instance's Pod waits for the
// OCI adapter.
func (r *InstanceReconciler) gatesForImageConversion(instance *computev1alpha.Instance) bool {
	if r.Config == nil || !r.Config.DownstreamResourceManagement.EnableImageConversion {
		return false
	}
	// Converted images are http+oci URLs only adapters may name; gating any
	// such image makes the adapter refuse it.
	return instance.Annotations[imageFormatAnnotation] != imageFormatUnikraft || hasHTTPOCIImage(instance)
}

func hasHTTPOCIImage(instance *computev1alpha.Instance) bool {
	for _, c := range instance.Spec.Runtime.Sandbox.Containers {
		if strings.HasPrefix(strings.ToLower(c.Image), "http+oci://") {
			return true
		}
	}
	return false
}

// imageConversionFailure reports why a gated Pod's images could not be
// converted, in the Instance's terms. The condition's message is detail for
// operators, so it is not shown.
func imageConversionFailure(instance *computev1alpha.Instance, pod *core.Pod) (string, bool) {
	c := imagesNotReady(pod)
	if c == nil || c.Reason == imageConverting {
		return "", false
	}
	image := "One of the instance's images"
	if sandbox := instance.Spec.Runtime.Sandbox; sandbox != nil && len(sandbox.Containers) == 1 {
		image = fmt.Sprintf("Image %q", sandbox.Containers[0].Image)
	}
	switch c.Reason {
	case imagePullFailed:
		return image + " could not be pulled" + pullCredentials(instance), true
	case imageUnsupportedPlatform:
		return image + " is not built for a supported platform", true
	}
	return image + " could not be prepared; retrying", true
}

// imageConversionInProgress reports whether a gated Pod's images are being
// converted.
func imageConversionInProgress(pod *core.Pod) bool {
	c := imagesNotReady(pod)
	return c != nil && c.Reason == imageConverting
}

// imagesNotReady returns a gated Pod's ImagesReady condition if it is False.
// The OCI adapter owns it only while the Pod is gated.
func imagesNotReady(pod *core.Pod) *core.PodCondition {
	if len(pod.Spec.SchedulingGates) == 0 {
		return nil
	}
	for i, c := range pod.Status.Conditions {
		if c.Type == imagesReadyCondition && c.Status == core.ConditionFalse {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
