package workerctl

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/ledartifact"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// AssembleBundle validates both architectures before publishing a catalog.
// Input artifact paths are local; published paths are content-addressed relative
// names. requireComplete is a packaging gate, never a qualification claim.
func AssembleBundle(directory string, inputs Catalog, requireComplete bool) (Catalog, []string, error) {
	return assembleBundle(directory, inputs, requireComplete, nil)
}
func assembleBundle(directory string, inputs Catalog, requireComplete bool, boundary func(string) error) (Catalog, []string, error) {
	var result Catalog
	if inputs.Version != 1 || !config.ValidInventoryName(inputs.Release) || len(inputs.Linux) != 2 {
		return result, nil, fmt.Errorf("bundle requires version 1, a named release and Linux amd64/arm64")
	}
	missing := []string{}
	for _, arch := range []string{"amd64", "arm64"} {
		a, ok := inputs.Linux[arch]
		if !ok {
			return result, nil, fmt.Errorf("missing %s bundle", arch)
		}
		if err := VerifyArtifact(a.Helper, arch, true); err != nil {
			return result, nil, fmt.Errorf("%s helper: %w", arch, err)
		}
		for _, v := range []struct{ name, value string }{{"backup_image", a.BackupImage}, {"relay_image", a.RelayImage}} {
			if v.value == "" {
				missing = append(missing, arch+"."+v.name)
			} else if err := config.PinnedWorkerImage(v.value); err != nil {
				return result, nil, fmt.Errorf("%s %s: %w", arch, v.name, err)
			}
		}
		if a.Signal == nil {
			missing = append(missing, arch+".signal")
		} else if err := VerifySignalArtifact(*a.Signal, arch); err != nil {
			return result, nil, fmt.Errorf("%s Signal: %w", arch, err)
		}
		if a.LEDs == nil {
			missing = append(missing, arch+".leds")
		} else if _, err := ledartifact.Verify(a.LEDs.File, a.LEDs.SHA256); err != nil {
			return result, nil, fmt.Errorf("%s LED archive: %w", arch, err)
		}
	}
	sort.Strings(missing)
	if requireComplete && len(missing) > 0 {
		return result, missing, fmt.Errorf("complete worker bundle is missing %v", missing)
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		return result, missing, err
	}
	d, err := safefs.Open(directory)
	if err != nil {
		return result, missing, err
	}
	defer d.Close()
	lock, err := d.Lock("bundle.lock", true, true)
	if err != nil {
		return result, missing, err
	}
	defer lock.Close()
	// Refuse foreign catalog entries; publication replaces only a regular file.
	if f, e := d.Open("catalog.json", unix.O_RDONLY|unix.O_NONBLOCK, 0); e == nil {
		f.Close()
	} else if !os.IsNotExist(e) {
		return result, missing, fmt.Errorf("existing bundle catalog must be a regular file without group/other write access: %w", e)
	}
	result = Catalog{Version: 1, Release: inputs.Release, Linux: map[string]ArchitectureArtifacts{}}
	for _, arch := range []string{"amd64", "arm64"} {
		a := inputs.Linux[arch]
		if a.Helper, err = copyBundleArtifact(d, a.Helper, "deploy-linux-"+arch, "", 128<<20, 0755); err != nil {
			return Catalog{}, missing, err
		}
		if a.Signal != nil {
			v, e := copyBundleArtifact(d, *a.Signal, "signal-linux-"+arch, "", 512<<20, 0755)
			if e != nil {
				return Catalog{}, missing, e
			}
			a.Signal = &v
		}
		if a.LEDs != nil {
			v, e := copyBundleArtifact(d, *a.LEDs, "leds-linux-"+arch, ".pyz", 8<<20, 0644)
			if e != nil {
				return Catalog{}, missing, e
			}
			a.LEDs = &v
		}
		result.Linux[arch] = a
		if boundary != nil {
			if err = boundary(arch); err != nil {
				return Catalog{}, missing, err
			}
		}
	}
	if err = d.WriteJSON("catalog.json", result, true); err != nil {
		return Catalog{}, missing, err
	}
	return result, missing, nil
}

func copyBundleArtifact(d *safefs.Directory, a Artifact, prefix, suffix string, limit int64, mode os.FileMode) (Artifact, error) {
	name := prefix + "-" + a.SHA256 + suffix
	if existing, err := d.Open(name, unix.O_RDONLY|unix.O_NONBLOCK, 0); err == nil {
		defer existing.Close()
		info, e := existing.Stat()
		if e != nil {
			return Artifact{}, e
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(existing, limit+1))
		if e != nil || n == 0 || n > limit || fmt.Sprintf("%x", h.Sum(nil)) != a.SHA256 || info.Mode().Perm() != mode {
			return Artifact{}, fmt.Errorf("existing bundle artifact changed")
		}
		return Artifact{name, a.SHA256}, nil
	} else if !os.IsNotExist(err) {
		return Artifact{}, err
	}
	input, err := os.Open(a.File)
	if err != nil {
		return Artifact{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return Artifact{}, fmt.Errorf("bundle source changed")
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return Artifact{}, err
	}
	tmp := ".bundle-" + id
	out, err := d.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return Artifact{}, err
	}
	defer unix.Unlinkat(int(d.File().Fd()), tmp, 0)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(input, limit+1))
	if err == nil && (n != info.Size() || n > limit || fmt.Sprintf("%x", h.Sum(nil)) != a.SHA256) {
		err = fmt.Errorf("bundle source changed during copy")
	}
	if err == nil {
		err = out.Chmod(mode)
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return Artifact{}, err
	}
	if err = d.Rename(tmp, d, name); err != nil {
		return Artifact{}, err
	}
	return Artifact{name, a.SHA256}, nil
}
