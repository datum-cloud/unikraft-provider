package imageconvert

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	core "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodImagePathIndex indexes Pods by the converted image paths they refer to.
const PodImagePathIndex = "imageconvert.datumapis.com/image-path"

// IndexPodImagePaths returns the index function for PodImagePathIndex.
func IndexPodImagePaths(advertiseURL string) client.IndexerFunc {
	return func(o client.Object) []string {
		var paths []string
		for _, c := range o.(*core.Pod).Spec.Containers {
			if p, ok := strings.CutPrefix(c.Image, advertiseURL); ok {
				paths = append(paths, p)
			}
		}
		return paths
	}
}

// Server serves converted images to the node's image agent over http+oci, by
// digest. The agent sends a HEAD at every instance start and a GET when it
// lacks the image; both are allowed only while a Pod refers to the image and
// its credentials may still pull the upstream image.
type Server struct {
	Converter *Converter
	// Pods reads Pods from the cache, indexed by PodImagePathIndex.
	Pods client.Reader
	// Secrets reads pull Secrets uncached.
	Secrets client.Reader
}

var errNoPod = errors.New("no pod refers to this image")

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.G(ctx).WithField("method", r.Method).WithField("path", r.URL.Path)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	layout, ok := layoutOf(r.URL.Path)
	if !ok {
		http.Error(w, "not a digest reference", http.StatusNotFound)
		return
	}
	img, creds, err := s.authorize(ctx, r.URL.Path, layout)
	if err != nil {
		logger.WithError(err).Info("refused")
		status := http.StatusForbidden
		if errors.Is(err, errNoPod) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	w.Header().Set("Content-Type", mediaTypeLayout)
	if rc, size, err := s.Converter.Open(ctx, img); err == nil {
		defer rc.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		s.copy(w, r, rc, logger)
		return
	}
	// A rebuild fails after the headers below; refuse one that can't work.
	if img.Source.Format != Format {
		http.Error(w, "converted with format "+img.Source.Format+", not "+Format, http.StatusNotFound)
		return
	}

	// Not on this node: rebuild it. The agent waits ten seconds for headers
	// but its whole image timeout for the body, so headers go out first.
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush() //nolint:errcheck
	if r.Method == http.MethodHead {
		go func() {
			if err := s.Converter.Ensure(context.WithoutCancel(ctx), img, creds); err != nil {
				logger.WithError(err).Error("rebuilding image")
			}
		}()
		return
	}
	if err := s.Converter.Ensure(ctx, img, creds); err != nil {
		logger.WithError(err).Error("rebuilding image")
		// A broken connection fails the agent's fetch outright.
		panic(http.ErrAbortHandler)
	}
	rc, _, err := s.Converter.Open(ctx, img)
	if err != nil {
		logger.WithError(err).Error("opening rebuilt image")
		panic(http.ErrAbortHandler)
	}
	defer rc.Close()
	s.copy(w, r, rc, logger)
}

func (s *Server) copy(w http.ResponseWriter, r *http.Request, rc io.Reader, logger *log.Entry) {
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.Copy(w, rc)
	logger.WithField("bytes", n).WithError(err).Info("served")
}

// authorize returns the image path names and credentials that may still pull
// its source, from a Pod that refers to it. Only adapters name converted
// images, so these are Pods an adapter checked.
func (s *Server) authorize(ctx context.Context, path string, layout digest.Digest) (Converted, Credentials, error) {
	var pods core.PodList
	if err := s.Pods.List(ctx, &pods, client.MatchingFields{PodImagePathIndex: path}); err != nil {
		return Converted{}, Credentials{}, err
	}
	err := errNoPod
	for i := range pods.Items {
		pod := &pods.Items[i]
		src, ok := sourcesOf(pod)[layout]
		if !ok {
			continue
		}
		named, perr := reference.ParseNamed(src.Image)
		if perr != nil {
			err = perr
			continue
		}
		keys, kerr := keyringFor(ctx, s.Secrets, pod)
		if kerr != nil {
			err = kerr
			continue
		}
		for _, creds := range keys.For(named) {
			if err = s.Converter.Authorize(ctx, src, creds); err == nil {
				return Converted{Source: src, Layout: layout}, creds, nil
			}
		}
	}
	return Converted{}, Credentials{}, err
}
