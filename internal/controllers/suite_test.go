package controllers_test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	functionsv1alpha1 "go.datum.net/ufo/pkg/apis/functions/v1alpha1"
	functionctrl "go.datum.net/ufo/internal/controllers/function"
	functionrevisionctrl "go.datum.net/ufo/internal/controllers/functionrevision"
	functionbuildctrl "go.datum.net/ufo/internal/controllers/functionbuild"
	functionrevisiongcctrl "go.datum.net/ufo/internal/controllers/functionrevisiongc"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "base", "crds"),
		},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("1.31.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Register all required schemes.
	err = functionsv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = computev1alpha.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = networkingv1alpha.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = batchv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = corev1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Start the controller manager with all four controllers.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	Expect(err).NotTo(HaveOccurred())

	err = (&functionctrl.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&functionrevisionctrl.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&functionbuildctrl.Reconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		KraftImage: "ghcr.io/unikraft/kraftkit:latest",
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&functionrevisiongcctrl.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = mgr.Start(ctx)
		Expect(err).NotTo(HaveOccurred())
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// helpers shared across controller test files

func ptr[T any](v T) *T {
	return &v
}

func newTestNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func newGitFunction(namespace, name string) *functionsv1alpha1.Function {
	return &functionsv1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: functionsv1alpha1.FunctionSpec{
			Source: functionsv1alpha1.FunctionSource{
				Git: &functionsv1alpha1.GitSource{
					URL: "https://github.com/example/my-function",
					Ref: functionsv1alpha1.GitRef{Branch: "main"},
				},
			},
			Runtime: functionsv1alpha1.FunctionRuntime{
				Language: "go",
				Port:     ptr(int32(8080)),
			},
		},
	}
}

func newImageFunction(namespace, name, imageRef string) *functionsv1alpha1.Function {
	return &functionsv1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: functionsv1alpha1.FunctionSpec{
			Source: functionsv1alpha1.FunctionSource{
				Image: &functionsv1alpha1.ImageSource{
					Ref: imageRef,
				},
			},
			Runtime: functionsv1alpha1.FunctionRuntime{
				Language: "go",
				Port:     ptr(int32(8080)),
			},
		},
	}
}

func newFunctionRevision(namespace, name, functionName, imageRef string) *functionsv1alpha1.FunctionRevision {
	return &functionsv1alpha1.FunctionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"functions.datumapis.com/function-name": functionName,
				"functions.datumapis.com/generation":    "1",
			},
		},
		Spec: functionsv1alpha1.FunctionRevisionSpec{
			FunctionSpec: functionsv1alpha1.FunctionSpec{
				Source: functionsv1alpha1.FunctionSource{
					Image: &functionsv1alpha1.ImageSource{Ref: imageRef},
				},
				Runtime: functionsv1alpha1.FunctionRuntime{
					Language: "go",
					Port:     ptr(int32(8080)),
				},
			},
			ImageRef: imageRef,
		},
	}
}

func newWorkloadAvailable(namespace, name, functionName string) *computev1alpha.Workload {
	return &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"functions.datumapis.com/function-name": functionName,
			},
		},
		Spec: computev1alpha.WorkloadSpec{
			Template: computev1alpha.InstanceTemplateSpec{
				Spec: computev1alpha.InstanceSpec{
					Runtime: computev1alpha.InstanceRuntimeSpec{
						Resources: computev1alpha.InstanceRuntimeResources{
							InstanceType: "datumcloud/d1-standard-2",
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						VirtualMachine: &computev1alpha.VirtualMachineRuntime{
							Ports: []computev1alpha.NamedPort{
								{Name: "http", Port: 8080},
							},
						},
					},
					NetworkInterfaces: []computev1alpha.InstanceNetworkInterface{
						{
							Network: networkingv1alpha.NetworkRef{
								Name:      "default",
								Namespace: namespace,
							},
						},
					},
					Volumes: []computev1alpha.InstanceVolume{
						{
							Name: "boot",
							VolumeSource: computev1alpha.VolumeSource{
								Disk: &computev1alpha.DiskTemplateVolumeSource{
									Template: &computev1alpha.DiskTemplateVolumeSourceTemplate{
										Spec: computev1alpha.DiskSpec{
											Resources: &computev1alpha.DiskResourceRequirements{
												Requests: corev1.ResourceList{
													corev1.ResourceStorage: resource.MustParse("1Gi"),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			Placements: []computev1alpha.WorkloadPlacement{
				{
					Name:      "default",
					CityCodes: []string{"ANY"},
					ScaleSettings: computev1alpha.HorizontalScaleSettings{
						MinReplicas: 0,
					},
				},
			},
		},
		Status: computev1alpha.WorkloadStatus{
			Replicas: 1,
			Conditions: []metav1.Condition{
				{
					Type:               computev1alpha.WorkloadAvailable,
					Status:             metav1.ConditionTrue,
					Reason:             "Available",
					Message:            "Workload is available",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
}
