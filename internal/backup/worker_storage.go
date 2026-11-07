package backup

import (
	"fmt"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

// Home storage creates only descendants of the trusted home. A customized
// outside-home archive requires an existing parent owned by the operator; a
// required external mount always uses its exact declared mountpoint.
func workerStorageBase(home string, s config.WorkerStorage, layouts ...InstallationPaths) (string, error) {
	if !config.WorkerAbsolutePath(s.Path) {
		return "", fmt.Errorf("invalid archive path")
	}
	if s.Mode != "home" {
		if (s.Mode != "" && s.Mode != "mount") || !config.WorkerAbsolutePath(s.RequireMount) || s.Path != s.RequireMount && !strings.HasPrefix(s.Path, s.RequireMount+"/") {
			return "", fmt.Errorf("invalid required archive mount")
		}
		return s.RequireMount, nil
	}
	if s.RequireMount != "" {
		return "", fmt.Errorf("home storage cannot specify require_mount")
	}
	if !config.WorkerAbsolutePath(home) {
		return "", fmt.Errorf("invalid target home")
	}
	if s.Path == home {
		return "", fmt.Errorf("archive must not replace the user's home")
	}
	if strings.HasPrefix(s.Path, home+"/") {
		return home, nil
	}
	if len(layouts) == 1 {
		p := layouts[0]
		if p.Home != home || p.Validate() != nil {
			return "", fmt.Errorf("invalid archive layout authority")
		}
		// XDG_DATA_HOME is an explicitly inspected target root. The default
		// archive may create its private descendants, while unrelated external
		// overrides still require an existing parent.
		if s.Path == filepath.Join(p.Data, "archive") && pathInside(p.Data, p.DataHome) {
			return p.DataHome, nil
		}
	}
	parent := filepath.Dir(s.Path)
	if parent == "/" {
		return "", fmt.Errorf("home storage requires an existing non-root parent")
	}
	return parent, nil
}

func workerMountPresent(data []byte, mount string) bool {
	if !config.WorkerAbsolutePath(mount) {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 6 {
			// mountinfo uses octal escaping, not shell quoting.
			actual := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
			if actual == mount {
				return true
			}
		}
	}
	return false
}

// Pin the filesystem of the nearest existing archive ancestor before creating
// directories. A later mount replacement cannot restart enrollment elsewhere.
func existingArchiveAncestor(archive string) (string, error) {
	if !config.WorkerAbsolutePath(archive) {
		return "", fmt.Errorf("invalid archive path")
	}
	for candidate := archive; candidate != "/"; candidate = filepath.Dir(candidate) {
		_, err := os.Lstat(candidate)
		if err == nil {
			dir, openErr := safefs.Open(candidate)
			if openErr != nil {
				return "", openErr
			}
			dir.Close()
			return candidate, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("archive requires an existing non-root parent")
}
func homeArchiveFilesystem(archive string) (string, error) {
	parent, err := existingArchiveAncestor(archive)
	if err != nil {
		return "", err
	}
	dir, err := safefs.Open(parent)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	return dir.FilesystemID()
}
