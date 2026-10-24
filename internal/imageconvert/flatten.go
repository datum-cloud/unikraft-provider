package imageconvert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/opencontainers/image-spec/identity"
)

// mkfsArgs make the EROFS image depend on the tree alone: no UUID, epoch build
// time. It stays uncompressed, as the runtime maps it into instances with DAX.
var mkfsArgs = []string{"--quiet", "-Uclear", "-T0", "--mkfs-time"}

// unpackAndFlatten writes img's root filesystem to out as one EROFS image: it
// unpacks img and runs mkfs.erofs on a throwaway writable snapshot of it.
func (c *Converter) unpackAndFlatten(ctx context.Context, img containerd.Image, out string) error {
	if err := img.Unpack(ctx, c.cfg.Snapshotter); err != nil {
		return fmt.Errorf("unpacking: %w", err)
	}
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return fmt.Errorf("reading the image's layers: %w", err)
	}
	sn := c.cfg.Client.SnapshotService(c.cfg.Snapshotter)
	key := snapshotPrefix + randomID()
	mounts, err := sn.Prepare(ctx, key, identity.ChainID(diffIDs).String())
	if err != nil {
		return fmt.Errorf("creating a snapshot of the image: %w", err)
	}
	defer sn.Remove(context.WithoutCancel(ctx), key) //nolint:errcheck // Cleanup removes leftovers.

	return mount.WithTempMount(ctx, mounts, func(root string) error {
		// The root directory's time is when this node unpacked the image, not
		// part of it, so it is reset for every node to build the same bytes.
		if err := os.Chtimes(root, epoch, epoch); err != nil {
			return err
		}
		prefetch(ctx, root, c.cfg.PrefetchWorkers)
		cmd := exec.CommandContext(ctx, c.cfg.MkfsErofs, append(mkfsArgs, out, root)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("mkfs.erofs: %w: %s", err, output)
		}
		return nil
	})
}

// prefetch reads every file under dir in parallel, which brings a cold tree
// into the page cache several times faster than mkfs.erofs reading serially.
func prefetch(ctx context.Context, dir string, workers int) {
	if workers < 1 {
		return
	}
	files := make(chan string, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for name := range files {
				if f, err := os.Open(name); err == nil {
					_, _ = io.Copy(io.Discard, f)
					f.Close()
				}
			}
		})
	}
	_ = filepath.WalkDir(dir, func(name string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files <- name
		}
		return ctx.Err()
	})
	close(files)
	wg.Wait()
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
