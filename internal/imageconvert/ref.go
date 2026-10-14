package imageconvert

import (
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// Converted images are served at /<source repository>/@<layout digest>, e.g.
// /docker.io/library/nginx/@sha256:6647…. The image agent parses the path as
// a repository, which cannot hold ':', so a registry's port follows '__',
// which hostnames cannot hold.
const portSeparator = "__"

// Source is what a converted image is built from.
type Source struct {
	// Image is the upstream image, pinned by digest.
	Image string `json:"image"`
	// Format is the conversion format.
	Format string `json:"format"`
}

// Converted is a converted image.
type Converted struct {
	Source Source
	// Layout is the served layout tar's digest.
	Layout digest.Digest
}

// imagePath is where a converted image of repo is served.
func imagePath(repo reference.Named, layout digest.Digest) string {
	host := strings.Replace(reference.Domain(repo), ":", portSeparator, 1)
	return "/" + host + "/" + reference.Path(repo) + "/@" + layout.String()
}

// layoutOf returns the layout digest an http+oci request path names.
func layoutOf(path string) (digest.Digest, bool) {
	_, layout, ok := strings.Cut(path, "/@")
	if !ok {
		return "", false
	}
	d, err := digest.Parse(layout)
	return d, err == nil
}
