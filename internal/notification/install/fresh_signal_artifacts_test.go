package install

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestFreshSignalArtifactCacheRevalidatesChanges(t *testing.T) {
	for _, change := range []string{"rewrite", "replace", "permissions", "hardlink", "symlink", "digest", "limit"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "signal-cli")
			raw := []byte("synthetic executable bytes")
			testutil.Must(t, os.WriteFile(path, raw, 0700))
			input := freshSignalInput{name: "signal-cli", hash: managedDigest(raw), mode: 0700, limit: 4096}
			cache := freshArtifactCache{}
			testutil.Must(t, cache.verify(root, input))
			testutil.Must(t, cache.verify(root, input))
			info, err := os.Stat(path)
			testutil.Must(t, err)
			switch change {
			case "rewrite":
				raw[0] ^= 1
				testutil.Must(t, os.WriteFile(path, raw, 0700))
				// Same size and restored mtime cannot hide an in-place rewrite.
				testutil.Must(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
			case "replace":
				testutil.Must(t, os.Remove(path))
				testutil.Must(t, os.WriteFile(path, []byte("replacement"), 0700))
			case "permissions":
				testutil.Must(t, os.Chmod(path, 0755))
			case "hardlink":
				testutil.Must(t, os.Link(path, filepath.Join(root, "other")))
			case "symlink":
				testutil.Must(t, os.Rename(path, filepath.Join(root, "other")))
				testutil.Must(t, os.Symlink("other", path))
			case "digest":
				input.hash = managedDigest([]byte("another pin"))
			case "limit":
				input.limit = 1
			}
			if err = cache.verify(root, input); err == nil {
				t.Fatal("cached artifact admitted changed file or pin")
			}
		})
	}
}

func TestFreshSignalArtifactCacheConcurrentAndNoSecretEntries(t *testing.T) {
	root := t.TempDir()
	raw := []byte("synthetic executable")
	testutil.Must(t, os.WriteFile(filepath.Join(root, "deploy"), raw, 0700))
	input := freshSignalInput{name: "deploy", hash: managedDigest(raw), mode: 0700, limit: 4096}
	cache := freshArtifactCache{}
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := cache.verify(root, input); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	secret := []byte("private selector")
	testutil.Must(t, os.WriteFile(filepath.Join(root, "account"), secret, 0600))
	testutil.Must(t, cache.verify(root, freshSignalInput{name: "account", hash: managedDigest(secret), mode: 0600, limit: 4096}))
	if len(cache.pins) != 1 {
		t.Fatal("secret cached or duplicate artifact entries")
	}
}
