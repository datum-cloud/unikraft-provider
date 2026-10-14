package imageconvert

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The runtime's image format: one uncompressed layer holding the EROFS root
// filesystem at unikraft/bin/initrd, selected from an index by platform. The
// runtime maps the initrd into every instance, so it stays uncompressed.
const (
	annotationInitrd = "org.unikraft.kernel.initrd"
	initrdPath       = "unikraft/bin/initrd"

	annotationSourceImage  = "compute.datumapis.com/source-image"
	annotationSourceDigest = "compute.datumapis.com/source-digest"
	annotationFormat       = "compute.datumapis.com/conversion-format"

	// mediaTypeLayout is what the image agent expects over http+oci.
	mediaTypeLayout = "application/vnd.oci.layout.v1+tar"
)

// epoch is every tar entry's time, so tars depend on content alone.
var epoch = time.Unix(0, 0).UTC()

type layoutInput struct {
	initrd   string
	config   ocispec.ImageConfig
	platform ocispec.Platform
	source   reference.Canonical
	format   string
}

// writeLayout writes a converted image as an OCI layout tar and returns its
// size. The image agent requires the tar rooted at ./, index.json listing
// the platform's manifest directly, and checks the tar's digest, so it is byte
// for byte deterministic.
func writeLayout(w io.Writer, in layoutInput) (int64, error) {
	// The config and manifest, which precede the layer in the tar, name its
	// digest, so the layer is produced twice: to hash it, then to write it.
	h := sha256.New()
	layerSize, err := writeLayer(h, in.initrd)
	if err != nil {
		return 0, err
	}
	layer := ocispec.Descriptor{
		MediaType:   ocispec.MediaTypeImageLayer,
		Digest:      digest.NewDigest(digest.SHA256, h),
		Size:        layerSize,
		Annotations: map[string]string{annotationInitrd: "/" + initrdPath},
	}
	config, err := json.Marshal(ocispec.Image{
		Platform: in.platform,
		Config: ocispec.ImageConfig{
			Entrypoint: in.config.Entrypoint,
			Cmd:        in.config.Cmd,
			Env:        in.config.Env,
			WorkingDir: in.config.WorkingDir,
			User:       in.config.User,
		},
		RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
	})
	if err != nil {
		return 0, err
	}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    descriptorOf(ocispec.MediaTypeImageConfig, config),
		Layers:    []ocispec.Descriptor{layer},
	})
	if err != nil {
		return 0, err
	}
	manifestDesc := descriptorOf(ocispec.MediaTypeImageManifest, manifest)
	manifestDesc.Platform = &in.platform
	index, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manifestDesc},
		Annotations: map[string]string{
			annotationSourceImage:  in.source.Name(),
			annotationSourceDigest: in.source.Digest().String(),
			annotationFormat:       in.format,
		},
	})
	if err != nil {
		return 0, err
	}

	cw := &countingWriter{w: w}
	t := &tarWriter{tw: tar.NewWriter(cw)}
	t.dir("./")
	t.file("./oci-layout", []byte(`{"imageLayoutVersion":"`+ocispec.ImageLayoutVersion+`"}`))
	t.file("./index.json", index)
	t.dir("./blobs/")
	t.dir("./blobs/sha256/")
	for _, blob := range [][]byte{index, manifest, config} {
		t.file("./blobs/sha256/"+digest.FromBytes(blob).Encoded(), blob)
	}
	if t.header("./blobs/sha256/"+layer.Digest.Encoded(), tar.TypeReg, layer.Size) {
		if n, err := writeLayer(t.tw, in.initrd); err != nil {
			t.err = err
		} else if n != layer.Size {
			t.err = fmt.Errorf("initrd changed while packaging it")
		}
	}
	if t.err == nil {
		t.err = t.tw.Close()
	}
	if t.err != nil {
		return 0, t.err
	}
	return cw.n, nil
}

// writeLayer writes the image's layer, a tar holding the initrd.
func writeLayer(w io.Writer, initrd string) (int64, error) {
	f, err := os.Open(initrd)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	cw := &countingWriter{w: w}
	t := &tarWriter{tw: tar.NewWriter(cw)}
	if t.header(initrdPath, tar.TypeReg, info.Size()) {
		_, t.err = io.Copy(t.tw, f)
	}
	if t.err == nil {
		t.err = t.tw.Close()
	}
	return cw.n, t.err
}

// tarWriter writes deterministic tar entries, keeping the first error.
type tarWriter struct {
	tw  *tar.Writer
	err error
}

func (t *tarWriter) header(name string, typ byte, size int64) bool {
	if t.err != nil {
		return false
	}
	mode := int64(0o644)
	if typ == tar.TypeDir {
		mode = 0o755
	}
	t.err = t.tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Mode: mode, Size: size, ModTime: epoch, Format: tar.FormatGNU})
	return t.err == nil
}

func (t *tarWriter) dir(name string) { t.header(name, tar.TypeDir, 0) }

func (t *tarWriter) file(name string, data []byte) {
	if t.header(name, tar.TypeReg, int64(len(data))) {
		_, t.err = t.tw.Write(data)
	}
}

func descriptorOf(mediaType string, data []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
