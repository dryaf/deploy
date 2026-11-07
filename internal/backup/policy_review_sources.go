package backup

import (
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/safefs"
)

// Legacy setup did not retain SourceKeys. Its source enrollment receipts still
// pin each private key. Review may verify that evidence without reserving a key,
// enrolling a source or writing a declaration. Trust files without an older hash
// must be at the exact managed path referenced by the unchanged owned runtime.
func inspectSetupSourcePins(root *safefs.Directory, j WorkerSetupJournal, c WorkerInstallation, paths InstallationPaths) (map[string]string, map[string]string, error) {
	keys, trust := map[string]string{}, map[string]string{}
	if len(j.SourceEnrollments) != len(c.Sources) {
		return nil, nil, fmt.Errorf("source enrollment history is incomplete")
	}
	view := j
	view.Declaration = &SetupDeclaration{Version: 1, Sources: map[string]string{}}
	if c.Paths != nil {
		if j.Declaration == nil {
			return nil, nil, fmt.Errorf("source review requires retained custom layout authority")
		}
		declaration := *j.Declaration
		declaration.Sources = map[string]string{}
		view.Declaration = &declaration
	}
	for _, s := range c.Sources {
		e, ok := j.SourceEnrollments[s.Alias]
		if !ok || e.Schema != 1 || e.WorkerID != c.WorkerID || e.SourceID != s.SourceID || e.Fingerprint == "" || s.KeyFile != filepath.Join(paths.Keys(), s.Alias+".key") || s.KnownHosts != filepath.Join(paths.Keys(), s.Alias+".known_hosts") {
			return nil, nil, fmt.Errorf("source identity or managed key paths differ from enrollment")
		}
		if pin := j.SourceKeys[s.Alias]; pin != "" && pin != e.Fingerprint {
			return nil, nil, fmt.Errorf("source key reservation differs from enrollment")
		}
		keys[s.Alias] = e.Fingerprint
		view.Declaration.Sources[s.Alias] = "review-only"
		path := paths.OwnedPath(s.KnownHosts)
		raw, err := readBackupOwnedFile(root, path, 16<<10)
		if err != nil || len(raw) == 0 {
			return nil, nil, fmt.Errorf("retained source trust unavailable")
		}
		digest := sharedSetupDigest(raw)
		if expected := j.OwnedFiles[path]; expected != "" && expected != digest {
			return nil, nil, fmt.Errorf("source trust differs from owned files")
		}
		trust[path] = digest
	}
	view.SourceKeys = keys
	for alias := range j.SourceKeys {
		if keys[alias] == "" {
			return nil, nil, fmt.Errorf("unexpected retained source key reservation")
		}
	}
	if err := verifySetupSourceKeys(root, view); err != nil {
		return nil, nil, err
	}
	return keys, trust, nil
}
