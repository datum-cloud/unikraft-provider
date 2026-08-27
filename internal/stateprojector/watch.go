// This file sets up the shared Kubernetes informer factory and wires its
// Pod/Namespace events into podIndex and namespaceIndex.

package stateprojector

import (
	"context"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// watchCluster maintains two indexes off a single shared informer factory:
// ukpd instance uuid -> (project, instance, resources), from a cluster-wide
// watch of provider Pods (they live on the Kraftlet virtual node, so
// node-scoping cannot apply); and namespace -> decoded Milo project id, from
// a cluster-wide watch of Namespaces — the project a Pod belongs to is not
// derivable from the Pod alone. Blocks until ctx is done.
func watchCluster(ctx context.Context, clientset kubernetes.Interface, pods *podIndex, namespaces *namespaceIndex) {
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	podInformer := factory.Core().V1().Pods().Informer()
	nsInformer := factory.Core().V1().Namespaces().Informer()

	// A denied watch (missing RBAC) would otherwise show up only as every
	// instance being unattributable, so name it at the source.
	watchErrHandler := func(_ context.Context, _ *cache.Reflector, err error) {
		pods.stats.watchErrors.Add(1)
		log.Printf("podwatch error=%v (check the ukp-state-projector-pod-reader ClusterRole/Binding)", err)
	}
	if err := podInformer.SetWatchErrorHandlerWithContext(watchErrHandler); err != nil {
		log.Printf("podwatch warn=set_error_handler err=%v", err)
	}
	if err := nsInformer.SetWatchErrorHandlerWithContext(watchErrHandler); err != nil {
		log.Printf("podwatch warn=set_error_handler err=%v", err)
	}

	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { pods.upsert(toPod(obj)) },
		UpdateFunc: func(_, newObj any) { pods.upsert(toPod(newObj)) },
		DeleteFunc: func(obj any) { pods.delete(toPod(obj)) },
	})
	if err != nil {
		log.Fatalf("podwatch fatal=add_event_handler err=%v", err)
	}
	_, err = nsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { namespaces.upsert(toNamespace(obj)) },
		UpdateFunc: func(_, newObj any) { namespaces.upsert(toNamespace(newObj)) },
		DeleteFunc: func(obj any) { namespaces.delete(toNamespace(obj)) },
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
		log.Printf("podwatch synced indexed_instances=%d indexed_projects=%d", pods.len(), namespaces.len())
	}()

	log.Printf("podwatch starting scope=cluster-wide resync=30s")
	factory.Start(ctx.Done())
	<-ctx.Done()
	log.Printf("podwatch stopped")
}

func toPod(obj any) *corev1.Pod {
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

func toNamespace(obj any) *corev1.Namespace {
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
