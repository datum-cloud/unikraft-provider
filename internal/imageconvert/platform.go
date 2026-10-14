package imageconvert

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Platform pairs a platform the Unikraft runtime runs with the OCI platform
// its images are converted from, e.g. kraftcloud/x86_64 from linux/amd64.
// Targets are configuration rather than constants: the runtime's platform
// names may change, and it may come to run more than one.
type Platform struct {
	// Target is the platform of converted images, as the runtime's image
	// agent selects it from an index. It is kept verbatim: runtime platform
	// names are not normalized the way OCI ones are (x86_64, not amd64).
	Target ocispec.Platform
	// Source is the platform of the upstream image a converted image is
	// built from.
	Source ocispec.Platform
}

// ParsePlatform parses "target=source", e.g. "kraftcloud/x86_64=linux/amd64".
func ParsePlatform(s string) (Platform, error) {
	target, source, ok := strings.Cut(s, "=")
	if !ok {
		return Platform{}, fmt.Errorf("platform %q: want target=source, e.g. kraftcloud/x86_64=linux/amd64", s)
	}
	parts := strings.Split(target, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Platform{}, fmt.Errorf("platform %q: target must be os/arch[/variant]", s)
	}
	p := Platform{Target: ocispec.Platform{OS: parts[0], Architecture: parts[1]}}
	if len(parts) == 3 {
		p.Target.Variant = parts[2]
	}
	src, err := platforms.Parse(source)
	if err != nil {
		return Platform{}, fmt.Errorf("platform %q: source: %w", s, err)
	}
	p.Source = platforms.Normalize(src)
	return p, nil
}

func (p Platform) String() string {
	return platforms.Format(p.Target) + "=" + platforms.Format(p.Source)
}

// isTarget reports whether an index entry's platform is p's target.
func (p Platform) isTarget(entry *ocispec.Platform) bool {
	return entry != nil && entry.OS == p.Target.OS &&
		entry.Architecture == p.Target.Architecture && entry.Variant == p.Target.Variant
}

// sourceIn reports whether an index has an entry p converts from. Entries
// without a platform count, as containerd's platform filter keeps them.
func (p Platform) sourceIn(index ocispec.Index) bool {
	m := platforms.OnlyStrict(p.Source)
	return slices.ContainsFunc(index.Manifests, func(d ocispec.Descriptor) bool {
		return d.Platform == nil || m.Match(*d.Platform)
	})
}

// checkSource returns an error unless an image's platform is one p converts
// from.
func (p Platform) checkSource(image ocispec.Platform) error {
	if platforms.OnlyStrict(p.Source).Match(image) {
		return nil
	}
	return p.errNoSource(image)
}

// errUnsupportedPlatform means an image has no variant this node converts from.
var errUnsupportedPlatform = errors.New("unsupported platform")

func (p Platform) errNoSource(found ...ocispec.Platform) error {
	var names []string
	for _, f := range found {
		if name := platforms.Format(f); f.OS != "unknown" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return fmt.Errorf("%w: image has no %s variant, only %s", errUnsupportedPlatform, platforms.Format(p.Source), strings.Join(names, ", "))
}

func platformsOf(index ocispec.Index) []ocispec.Platform {
	var ps []ocispec.Platform
	for _, m := range index.Manifests {
		if m.Platform != nil {
			ps = append(ps, *m.Platform)
		}
	}
	return ps
}

// Platforms is the configured set of platforms.
type Platforms []Platform

// ForNode returns the platform this node converts to: the first whose source
// platform the node runs natively.
func (ps Platforms) ForNode() (Platform, error) {
	host := platforms.Only(platforms.DefaultSpec())
	for _, p := range ps {
		if host.Match(p.Source) {
			return p, nil
		}
	}
	return Platform{}, fmt.Errorf("no configured platform converts from this node's platform %s", platforms.DefaultString())
}

// targetIn reports whether any entry of an index is one of the configured
// targets, i.e. the image already runs on the Unikraft runtime as it is.
func (ps Platforms) targetIn(index ocispec.Index) bool {
	for _, m := range index.Manifests {
		for _, p := range ps {
			if p.isTarget(m.Platform) {
				return true
			}
		}
	}
	return false
}
