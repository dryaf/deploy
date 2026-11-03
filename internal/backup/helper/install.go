package helper

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type HelperArtifactManifest struct {
	Schema   int    `json:"schema"`
	Role     string `json:"role"`
	Current  string `json:"current"`
	Previous string `json:"previous,omitempty"`
	Pending  string `json:"pending,omitempty"`
}

func InstallSelf(home string, role string) error {
	if role != "backup-worker" && role != "backup-source" {
		return fmt.Errorf("invalid helper role")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	relative := ".local/lib/deploy/" + role
	if err = root.Mkdir(relative); err != nil {
		return err
	}
	dir, err := root.Sub(relative)
	if err != nil {
		return err
	}
	defer dir.Close()
	source, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<20 {
		return fmt.Errorf("invalid helper artifact")
	}
	digest, err := safefs.DigestFile(source)
	if err != nil {
		return err
	}
	var manifest HelperArtifactManifest
	manifestErr := dir.ReadJSON("artifact.json", &manifest)
	current, readErr := dir.Open("deploy", unix.O_RDONLY, 0)
	actual := ""
	if readErr == nil {
		actual, err = safefs.DigestFile(current)
		current.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if errors.Is(manifestErr, os.ErrNotExist) {
		if readErr == nil {
			return fmt.Errorf("refusing to replace an unowned helper executable")
		}
		manifest = HelperArtifactManifest{Schema: 1, Role: role}
	} else if manifestErr != nil {
		return manifestErr
	} else if manifest.Schema != 1 || manifest.Role != role {
		return fmt.Errorf("unsupported helper artifact ownership")
	}
	if actual != "" && actual != manifest.Current && actual != manifest.Pending {
		return fmt.Errorf("installed helper differs from owned artifact evidence")
	}
	if manifest.Pending != "" {
		if actual == manifest.Pending {
			manifest.Previous = manifest.Current
			manifest.Current = manifest.Pending
			manifest.Pending = ""
			if err = dir.WriteJSON("artifact.json", manifest, true); err != nil {
				return err
			}
		} else if digest != manifest.Pending {
			return fmt.Errorf("an interrupted helper upgrade requires its original artifact")
		}
	}
	if actual == digest && manifest.Current == digest {
		return nil
	}
	release := "releases/" + digest
	if err = dir.Mkdir(release); err != nil {
		return err
	}
	releaseDir, err := dir.Sub(release)
	if err != nil {
		return err
	}
	defer releaseDir.Close()
	existing, err := releaseDir.Open("deploy", unix.O_RDONLY, 0)
	if err == nil {
		got, e := safefs.DigestFile(existing)
		existing.Close()
		if e != nil || got != digest {
			return fmt.Errorf("immutable helper artifact conflict")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err = CopyHelperFile(source, releaseDir, "deploy", false); err != nil {
			return err
		}
	} else {
		return err
	}
	manifest.Pending = digest
	if err = dir.WriteJSON("artifact.json", manifest, manifestErr == nil); err != nil {
		return err
	}
	if err = CopyHelperFile(source, dir, "deploy", readErr == nil); err != nil {
		return err
	}
	manifest.Previous = manifest.Current
	manifest.Current = digest
	manifest.Pending = ""
	return dir.WriteJSON("artifact.json", manifest, true)
}

func CopyHelperFile(source *os.File, dir *safefs.Directory, name string, replace bool) error {
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	temp := "." + id
	dest, err := dir.Open(temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0700)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(dir.File().Fd()), temp, 0)
	if _, err = source.Seek(0, io.SeekStart); err == nil {
		_, err = io.Copy(dest, io.LimitReader(source, 128<<20+1))
	}
	if err == nil {
		err = dest.Sync()
	}
	closeErr := dest.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	flags := uint(safefs.RenameNoReplace)
	if replace {
		flags = 0
	}
	if err = safefs.RenameAt(int(dir.File().Fd()), temp, int(dir.File().Fd()), name, flags); err != nil {
		return err
	}
	return dir.File().Sync()
}
