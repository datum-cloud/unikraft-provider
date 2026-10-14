// Package imageconvert lets the Unikraft runtime run plain OCI images.
//
// The runtime boots only its own format: an index entry for the runtime's
// platform whose single layer holds the root filesystem as an EROFS image. An
// adapter on each node pulls an image through the node's containerd, flattens
// its snapshot with mkfs.erofs and serves the result to the runtime's image
// agent over http+oci, by digest. Conversion is deterministic, so every node
// builds the same bytes for the same image.
//
// The provider creates Pods with a scheduling gate; PodReconciler converts
// their images, points the Pod at them and removes the gate. Server serves the
// images on the node the Pod lands on, rebuilding any it does not hold, and
// checks at every instance start that the Pod may still pull the upstream
// image.
package imageconvert

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/util/cache"
)

// Format versions the conversion. Bump it whenever conversion changes the
// bytes it produces, so old and new images never share a digest.
const Format = "v1"

const (
	// storeNamespace holds converted images in the node's containerd, apart
	// from the kubelet's.
	storeNamespace = "ukp-oci-adapter"
	// snapshotPrefix starts the key of every snapshot the converter mounts.
	snapshotPrefix = "ukp-oci-adapter/build/"
	// labelOwner marks the converter's leases, so Cleanup can find them.
	labelOwner = "datum.net/owner"
	owner      = "ukp-oci-adapter"
	// labelUsed is set when an image is served, which bumps its record's
	// UpdatedAt for eviction.
	labelUsed = "datum.net/last-used"
)

// Config configures a Converter.
type Config struct {
	// Client connects to the node's containerd, defaulting to the kubelet's
	// namespace so pulls share its cache.
	Client *containerd.Client
	// Snapshotter is the snapshotter images are unpacked into.
	Snapshotter string
	// MkfsErofs is the mkfs.erofs binary.
	MkfsErofs string
	// PrefetchWorkers read an image's files in parallel before mkfs.erofs.
	PrefetchWorkers int
	// MaxConversions is the most conversions run at once; others wait.
	MaxConversions int64
	// StoreMax is the most bytes of converted images kept; least recently
	// used ones are evicted first.
	StoreMax int64
	// Egress guards the connections to upstream registries.
	Egress Egress
	// Platforms are the runtime's platforms and what they convert from.
	Platforms Platforms
	// AuthorizeTTL is how long a successful upstream access check is reused.
	AuthorizeTTL time.Duration
}

// Converter converts OCI images to the runtime's format.
type Converter struct {
	cfg        Config
	platform   Platform
	builds     singleflight.Group
	slots      *semaphore.Weighted
	authorized *cache.Expiring
}

// New returns a converter for this node's platform.
func New(cfg Config) (*Converter, error) {
	p, err := cfg.Platforms.ForNode()
	if err != nil {
		return nil, err
	}
	if cfg.MaxConversions < 1 {
		return nil, errors.New("MaxConversions must be at least 1")
	}
	return &Converter{cfg: cfg, platform: p, slots: semaphore.NewWeighted(cfg.MaxConversions), authorized: cache.NewExpiring()}, nil
}

// Platform is the platform this converter builds for.
func (c *Converter) Platform() Platform { return c.platform }

// Prepared is how a container should refer to its image.
type Prepared struct {
	// Preconverted means the image runs on the runtime as it is.
	Preconverted bool
	// Converted is the converted image, unless Preconverted.
	Converted Converted
	// Path is where Converted is served.
	Path string
}

// errUnauthorized means credentials can't pull an image. Registries may answer
// that as not found.
var errUnauthorized = errors.New("not authorized")

// Prepare resolves ref with a namespace's credentials and converts it, unless
// it already runs on the runtime.
func (c *Converter) Prepare(ctx context.Context, ref string, keys *Keyring) (Prepared, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return Prepared{}, fmt.Errorf("invalid image %q: %w", ref, err)
	}
	named = reference.TagNameOnly(named)
	for _, creds := range keys.For(named) {
		desc, index, err := c.resolve(ctx, named.String(), creds)
		if errors.Is(err, errUnauthorized) {
			continue
		}
		if err != nil {
			return Prepared{}, err
		}
		if index != nil && c.cfg.Platforms.targetIn(*index) {
			return Prepared{Preconverted: true}, nil
		}
		if index != nil && !c.platform.sourceIn(*index) {
			return Prepared{}, c.platform.errNoSource(platformsOf(*index)...)
		}
		src := Source{Image: named.Name() + "@" + desc.Digest.String(), Format: Format}
		target, err := c.build(ctx, src, creds)
		if err != nil {
			return Prepared{}, err
		}
		c.authorized.Set(authorizeKey{src.Image, creds}, true, c.cfg.AuthorizeTTL)
		return Prepared{Converted: Converted{Source: src, Layout: target.Digest}, Path: imagePath(named, target.Digest)}, nil
	}
	return Prepared{}, fmt.Errorf("image %q not found, or %w with the namespace's pull credentials or anonymously", ref, errUnauthorized)
}

// head resolves ref with creds.
func (c *Converter) head(ctx context.Context, ref string, creds Credentials) (remotes.Resolver, ocispec.Descriptor, error) {
	resolver := c.cfg.Egress.resolver(creds)
	_, desc, err := resolver.Resolve(ctx, ref)
	if isUnauthorized(err) {
		return nil, ocispec.Descriptor{}, errUnauthorized
	}
	if err != nil {
		return nil, ocispec.Descriptor{}, fmt.Errorf("resolving %s: %w", ref, err)
	}
	return resolver, desc, nil
}

// resolve resolves ref with creds, returning its index if it names one.
func (c *Converter) resolve(ctx context.Context, ref string, creds Credentials) (ocispec.Descriptor, *ocispec.Index, error) {
	resolver, desc, err := c.head(ctx, ref, creds)
	if err != nil || !images.IsIndexType(desc.MediaType) {
		return desc, nil, err
	}
	fetcher, err := resolver.Fetcher(ctx, ref)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	rc, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("fetching the index of %s: %w", ref, err)
	}
	defer rc.Close()
	var index ocispec.Index
	if err := json.NewDecoder(rc).Decode(&index); err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("decoding the index of %s: %w", ref, err)
	}
	return desc, &index, nil
}

func isUnauthorized(err error) bool {
	var status remoteerrors.ErrUnexpectedStatus
	if errors.As(err, &status) {
		return status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden
	}
	return errors.Is(err, docker.ErrInvalidAuthorization) || errdefs.IsUnauthorized(err) ||
		errdefs.IsPermissionDenied(err) || errdefs.IsNotFound(err)
}

// Ensure makes sure img is stored, rebuilding it if not and checking the
// rebuild has img's digest.
func (c *Converter) Ensure(ctx context.Context, img Converted, creds Credentials) error {
	if _, err := c.cfg.Client.ContentStore().Info(c.storeCtx(ctx), img.Layout); err == nil {
		return nil
	}
	target, err := c.build(ctx, img.Source, creds)
	if err != nil {
		return err
	}
	if target.Digest != img.Layout {
		return fmt.Errorf("rebuilding %s produced %s, not %s", img.Source.Image, target.Digest, img.Layout)
	}
	return nil
}

// Open opens img if stored.
func (c *Converter) Open(ctx context.Context, img Converted) (io.ReadCloser, int64, error) {
	ctx = c.storeCtx(ctx)
	cs := c.cfg.Client.ContentStore()
	info, err := cs.Info(ctx, img.Layout)
	if err != nil {
		return nil, 0, err
	}
	ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: info.Digest, Size: info.Size})
	if err != nil {
		return nil, 0, err
	}
	used := images.Image{Name: c.recordName(img.Source), Labels: map[string]string{labelUsed: time.Now().UTC().Format(time.RFC3339)}}
	_, _ = c.cfg.Client.ImageService().Update(ctx, used, "labels."+labelUsed)
	return struct {
		io.Reader
		io.Closer
	}{bufio.NewReaderSize(content.NewReader(ra), 1<<20), ra}, info.Size, nil
}

// Authorize checks creds may pull src's upstream image.
func (c *Converter) Authorize(ctx context.Context, src Source, creds Credentials) error {
	key := authorizeKey{src.Image, creds}
	if _, ok := c.authorized.Get(key); ok {
		return nil
	}
	if _, _, err := c.head(ctx, key.ref, creds); err != nil {
		return err
	}
	c.authorized.Set(key, true, c.cfg.AuthorizeTTL)
	return nil
}

type authorizeKey struct {
	ref   string
	creds Credentials
}

// build converts src unless stored, returning the stored layout tar's
// descriptor. Concurrent builds of one image are shared, and outlive the
// requests that started them.
func (c *Converter) build(ctx context.Context, src Source, creds Credentials) (ocispec.Descriptor, error) {
	if src.Format != Format {
		return ocispec.Descriptor{}, fmt.Errorf("%s was converted with format %s; this converter builds %s", src.Image, src.Format, Format)
	}
	if img, err := c.cfg.Client.ImageService().Get(c.storeCtx(ctx), c.recordName(src)); err == nil {
		return img.Target, nil
	}
	v, err, _ := c.builds.Do(c.recordName(src), func() (any, error) {
		ctx := context.WithoutCancel(ctx)
		if err := c.slots.Acquire(ctx, 1); err != nil {
			return nil, err
		}
		defer c.slots.Release(1)
		start := time.Now()
		target, err := c.convert(ctx, src, creds)
		log.G(ctx).WithFields(log.Fields{"source": src.Image, "digest": target.Digest, "took": time.Since(start).Round(time.Millisecond)}).WithError(err).Info("converted image")
		return target, err
	})
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	return v.(ocispec.Descriptor), nil
}

func (c *Converter) convert(ctx context.Context, src Source, creds Credentials) (ocispec.Descriptor, error) {
	named, err := reference.ParseNamed(src.Image)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	source, ok := named.(reference.Canonical)
	if !ok {
		return ocispec.Descriptor{}, fmt.Errorf("%s is not pinned by digest", src.Image)
	}
	ctx, done, err := c.withLease(ctx)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer done()

	img, err := c.fetch(ctx, src.Image, c.cfg.Egress.resolver(creds))
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pulling %s: %w", src.Image, err)
	}
	spec, err := img.Spec(ctx)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	// containerd checks the platform of index entries only.
	if err := c.platform.checkSource(spec.Platform); err != nil {
		return ocispec.Descriptor{}, err
	}

	tmpdir, err := os.MkdirTemp("", "build-")
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer os.RemoveAll(tmpdir)
	initrd := filepath.Join(tmpdir, "initrd.erofs")
	if err := c.unpackAndFlatten(ctx, img, initrd); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("building %s: %w", src.Image, err)
	}
	return c.put(ctx, src, func(w io.Writer) (int64, error) {
		return writeLayout(w, layoutInput{initrd: initrd, config: spec.Config, platform: c.platform.Target, source: source, format: src.Format})
	})
}

// fetch fetches ref for this node's platform the way containerd's Pull does,
// sharing content with the kubelet, but creates no image record: records in
// the kubelet's namespace are the kubelet's. The caller's lease holds the
// content.
func (c *Converter) fetch(ctx context.Context, ref string, resolver remotes.Resolver) (containerd.Image, error) {
	name, desc, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return nil, err
	}
	cs := c.cfg.Client.ContentStore()
	platform := platforms.OnlyStrict(c.platform.Source)
	children := images.SetChildrenMappedLabels(cs,
		images.LimitManifests(images.FilterPlatforms(images.ChildrenHandler(cs), platform), platform, 1), nil)
	source, err := docker.AppendDistributionSourceLabel(cs, name)
	if err != nil {
		return nil, err
	}
	if err := images.Dispatch(ctx, images.Handlers(remotes.FetchHandler(cs, fetcher), source, children), nil, desc); err != nil {
		return nil, err
	}
	return containerd.NewImageWithPlatform(c.cfg.Client, images.Image{Name: name, Target: desc}, platform), nil
}

// Converted images are kept in storeNamespace, each as one blob, the layout
// tar, held by an image record named after what it was built from. Records
// past StoreMax are deleted least recently used first, and containerd's
// garbage collector removes their blobs.

func (c *Converter) storeCtx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, storeNamespace)
}

func (c *Converter) recordName(src Source) string {
	key := src.Format + "|" + platforms.Format(c.platform.Target) + "|" + src.Image
	return "converted/" + digest.FromString(key).Encoded()
}

// put stores the layout tar write produces as src's converted image.
func (c *Converter) put(ctx context.Context, src Source, write func(io.Writer) (int64, error)) (ocispec.Descriptor, error) {
	ctx, done, err := c.withLease(c.storeCtx(ctx))
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer done()

	w, err := content.OpenWriter(ctx, c.cfg.Client.ContentStore(), content.WithRef(c.recordName(src)))
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer w.Close()
	// Each write is a round trip to containerd, which takes up to 8MiB at once.
	bw := bufio.NewWriterSize(w, 8<<20)
	size, err := write(bw)
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	// containerd hashes what it stores; its digest is the image's.
	if err := w.Commit(ctx, size, ""); err != nil && !errdefs.IsAlreadyExists(err) {
		return ocispec.Descriptor{}, err
	}
	target := ocispec.Descriptor{MediaType: mediaTypeLayout, Digest: w.Digest(), Size: size}
	record := images.Image{Name: c.recordName(src), Target: target}
	if _, err := c.cfg.Client.ImageService().Create(ctx, record); errdefs.IsAlreadyExists(err) {
		_, err = c.cfg.Client.ImageService().Update(ctx, record, "target")
	} else if err != nil {
		return ocispec.Descriptor{}, err
	}
	c.evict(ctx)
	return target, nil
}

// evict deletes the least recently used images past StoreMax.
func (c *Converter) evict(ctx context.Context) {
	is := c.cfg.Client.ImageService()
	records, err := is.List(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("listing converted images")
		return
	}
	var total int64
	for _, r := range records {
		total += r.Target.Size
	}
	slices.SortFunc(records, func(a, b images.Image) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	for _, r := range records {
		if total <= c.cfg.StoreMax {
			return
		}
		if err := is.Delete(ctx, r.Name); err == nil {
			total -= r.Target.Size
		}
	}
}

// withLease keeps what ctx's operations create from garbage collection until
// done. Leases expire, and Cleanup removes those of an adapter that died.
func (c *Converter) withLease(ctx context.Context) (context.Context, func(), error) {
	ls := c.cfg.Client.LeasesService()
	l, err := ls.Create(ctx, leases.WithRandomID(), leases.WithExpiration(time.Hour), leases.WithLabel(labelOwner, owner))
	if err != nil {
		return nil, nil, err
	}
	return leases.WithLease(ctx, l.ID), func() { _ = ls.Delete(context.WithoutCancel(ctx), l) }, nil
}

// Cleanup removes what an adapter that died mid-build left behind: its
// leases and snapshots.
func (c *Converter) Cleanup(ctx context.Context) error {
	ls := c.cfg.Client.LeasesService()
	filter := "labels." + strconv.Quote(labelOwner) + "==" + owner
	for _, nsCtx := range []context.Context{ctx, c.storeCtx(ctx)} {
		stale, err := ls.List(nsCtx, filter)
		if err != nil {
			return fmt.Errorf("listing leases: %w", err)
		}
		for _, l := range stale {
			if err := ls.Delete(nsCtx, l); err != nil && !errdefs.IsNotFound(err) {
				return fmt.Errorf("deleting lease %s: %w", l.ID, err)
			}
		}
	}
	sn := c.cfg.Client.SnapshotService(c.cfg.Snapshotter)
	var stale []string
	if err := sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if strings.HasPrefix(info.Name, snapshotPrefix) {
			stale = append(stale, info.Name)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("listing snapshots: %w", err)
	}
	for _, v := range stale {
		if err := sn.Remove(ctx, v); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("removing snapshot %s: %w", v, err)
		}
	}
	return nil
}
