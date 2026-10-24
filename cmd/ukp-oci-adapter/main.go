// Command ukp-oci-adapter lets Unikraft instances run plain OCI images. It
// runs on every Unikraft node, converts the images of Pods the provider gates
// and serves them to the runtime's image agent over http+oci. See
// internal/imageconvert.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/log"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.datum.net/unikraft-provider/internal/imageconvert"
)

type options struct {
	listen        string
	advertiseURL  string
	node          string
	containerd    string
	namespace     string
	snapshotter   string
	mkfsErofs     string
	prefetch      int
	conversions   int64
	storeMax      resource.Quantity
	platforms     imageconvert.Platforms
	claimTTL      time.Duration
	preferredNode string
	authorizeTTL  time.Duration
	egress        imageconvert.Egress
}

func main() {
	o := options{storeMax: resource.MustParse("50Gi")}
	flag.StringVar(&o.listen, "listen", ":8090", "address the http+oci server listens on")
	flag.StringVar(&o.advertiseURL, "advertise-url", "", "http+oci URL the runtime's image agent reaches adapters at, the same on every node; converted images are named under it")
	flag.StringVar(&o.node, "node-name", os.Getenv("NODE_NAME"), "this node's name, recorded in claims")
	flag.StringVar(&o.containerd, "containerd-address", "/run/containerd/containerd.sock", "the node's containerd socket")
	flag.StringVar(&o.namespace, "containerd-namespace", "k8s.io", "containerd namespace to pull into, the kubelet's to share its cache")
	flag.StringVar(&o.snapshotter, "snapshotter", "overlayfs", "snapshotter to unpack images into")
	flag.StringVar(&o.mkfsErofs, "mkfs-erofs", "/usr/bin/mkfs.erofs", "mkfs.erofs binary")
	flag.IntVar(&o.prefetch, "prefetch-workers", 16, "files read in parallel before mkfs.erofs")
	flag.Int64Var(&o.conversions, "max-conversions", 4, "most images converted at once; others wait")
	flag.Func("store-max", "most space converted images take, least recently used evicted first (default 50Gi)", func(s string) (err error) {
		o.storeMax, err = resource.ParseQuantity(s)
		return err
	})
	flag.Func("platform", "runtime platform and the OCI platform it is built from, as target=source; repeatable (default kraftcloud/x86_64=linux/amd64)", func(s string) error {
		p, err := imageconvert.ParsePlatform(s)
		o.platforms = append(o.platforms, p)
		return err
	})
	flag.DurationVar(&o.claimTTL, "claim-ttl", 10*time.Minute, "how long a claim keeps other adapters off a Pod")
	flag.StringVar(&o.preferredNode, "preferred-node", "", "node released Pods prefer, the one this adapter's runtime serves; none if empty")
	flag.DurationVar(&o.authorizeTTL, "authorize-ttl", 30*time.Second, "how long a successful upstream access check is reused")
	flag.Func("insecure-registry", "registry host[:port] reached over plain HTTP, exempt from address checks; repeatable, for testing", func(s string) error {
		o.egress.Insecure = append(o.egress.Insecure, s)
		return nil
	})
	flag.Func("deny-cidr", "registry addresses refused besides loopback, private, link-local and the node's own; repeatable", func(s string) error {
		p, err := netip.ParsePrefix(s)
		o.egress.Denied = append(o.egress.Denied, p)
		return err
	})
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	ctx := ctrl.SetupSignalHandler()
	if err := run(ctx, o); err != nil {
		log.G(ctx).WithError(err).Error("exiting")
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) error {
	if !strings.HasPrefix(o.advertiseURL, "http+oci://") {
		return errors.New("-advertise-url must be an http+oci:// URL")
	}
	if o.node == "" {
		return errors.New("-node-name or NODE_NAME is required")
	}
	if len(o.platforms) == 0 {
		p, _ := imageconvert.ParsePlatform("kraftcloud/x86_64=linux/amd64")
		o.platforms = imageconvert.Platforms{p}
	}
	local, err := imageconvert.LocalPrefixes()
	if err != nil {
		return fmt.Errorf("listing the node's addresses: %w", err)
	}
	o.egress.Denied = append(o.egress.Denied, local...)

	client, err := containerd.New(o.containerd, containerd.WithDefaultNamespace(o.namespace))
	if err != nil {
		return fmt.Errorf("connecting to containerd: %w", err)
	}
	defer client.Close()
	conv, err := imageconvert.New(imageconvert.Config{
		Client:          client,
		Snapshotter:     o.snapshotter,
		MkfsErofs:       o.mkfsErofs,
		PrefetchWorkers: o.prefetch,
		MaxConversions:  o.conversions,
		StoreMax:        o.storeMax.Value(),
		Egress:          o.egress,
		Platforms:       o.platforms,
		AuthorizeTTL:    o.authorizeTTL,
	})
	if err != nil {
		return err
	}
	if err := conv.Cleanup(ctx); err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}
	advertiseURL := strings.TrimSuffix(o.advertiseURL, "/")
	if err := mgr.GetFieldIndexer().IndexField(ctx, &core.Pod{}, imageconvert.PodImagePathIndex, imageconvert.IndexPodImagePaths(advertiseURL)); err != nil {
		return fmt.Errorf("indexing pods: %w", err)
	}
	if err := (&imageconvert.PodReconciler{
		Client:        mgr.GetClient(),
		Secrets:       mgr.GetAPIReader(),
		Converter:     conv,
		Node:          o.node,
		AdvertiseURL:  advertiseURL,
		ClaimTTL:      o.claimTTL,
		PreferredNode: o.preferredNode,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              o.listen,
		Handler:           &imageconvert.Server{Converter: conv, Pods: mgr.GetClient(), Secrets: mgr.GetAPIReader()},
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		srv.BaseContext = func(net.Listener) context.Context { return ctx }
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		log.G(ctx).WithFields(log.Fields{"listen": o.listen, "platform": conv.Platform(), "format": imageconvert.Format}).Info("serving")
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
