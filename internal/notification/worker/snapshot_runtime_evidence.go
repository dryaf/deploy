package worker

import (
	"context"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Retain only worker-owned audit records, never credentials, sockets, SQLite
// sidecars or arbitrary files a caller may have put in the state directory.
func snapshotRuntimePath(name string) bool {
	if path.Clean(name) != name || path.IsAbs(name) || strings.ContainsAny(name, "\x00\r\n") || !strings.HasSuffix(name, ".json") || safefs.IsJSONTemporary(path.Base(name)) {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) == 1 {
		for _, prefix := range []string{"owner-", "managed-delivery-", "managed-telegram-handoff-", "telegram-renewal-", "led-runtime-clear-"} {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	return len(parts) == 2 && (parts[0] == "managed-commands" || parts[0] == "administration") || len(parts) == 3 && parts[0] == "ownership"
}

func snapshotRuntimeInventory(ctx context.Context, d *safefs.Directory, c Config) ([]recoveryEvidenceFile, error) {
	var entries []recoveryEvidenceFile
	remaining := c.MaxDatabaseBytes
	var scan func(*safefs.Directory, string, int) error
	scan = func(current *safefs.Directory, prefix string, depth int) error {
		if depth > 2 {
			return fmt.Errorf("snapshot audit directory nesting exceeds limit")
		}
		for {
			batch, err := current.File().ReadDir(128)
			for _, entry := range batch {
				if e := ctx.Err(); e != nil {
					return e
				}
				name := path.Join(prefix, entry.Name())
				if entry.IsDir() && (name == "managed-commands" || name == "administration" || name == "ownership" || strings.HasPrefix(name, "ownership/") && depth == 1) {
					child, e := current.Sub(entry.Name())
					if e != nil {
						return e
					}
					e = scan(child, name, depth+1)
					child.Close()
					if e != nil {
						return e
					}
					continue
				}
				if !snapshotRuntimePath(name) {
					continue
				}
				f, e := snapshotFile(d, name, safefs.MaxRecordBytes)
				if e != nil {
					return e
				}
				info, e := f.Stat()
				if e != nil {
					f.Close()
					return e
				}
				hash, e := boundedEvidenceDigest(f, info.Size())
				f.Close()
				if e != nil {
					return e
				}
				if info.Size() > remaining || len(entries) >= 8192 {
					return fmt.Errorf("snapshot runtime evidence exceeds budget")
				}
				remaining -= info.Size()
				entries = append(entries, recoveryEvidenceFile{name, info.Size(), hash})
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	scope, err := d.Sub(".")
	if err != nil {
		return nil, err
	}
	defer scope.Close()
	if err = scan(scope, "", 0); err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b recoveryEvidenceFile) int { return strings.Compare(a.Path, b.Path) })
	return entries, nil
}

func validateSnapshotRuntimeInventory(files []recoveryEvidenceFile, c Config) error {
	remaining, previous := c.MaxDatabaseBytes, ""
	if len(files) > 8192 {
		return fmt.Errorf("snapshot audit file budget exceeded")
	}
	for _, f := range files {
		if !snapshotRuntimePath(f.Path) || f.Path <= previous || !safefs.DigestPattern.MatchString(f.Hash) || f.Bytes <= 0 || f.Bytes > safefs.MaxRecordBytes || f.Bytes > remaining {
			return fmt.Errorf("invalid snapshot audit evidence")
		}
		previous = f.Path
		remaining -= f.Bytes
	}
	return nil
}

func retainOriginalRuntime(ctx context.Context, source, stage *safefs.Directory, c Config) (string, error) {
	files, err := snapshotRuntimeInventory(ctx, source, c)
	if err != nil {
		return "", err
	}
	if err = stage.Mkdir("original-runtime"); err != nil {
		return "", err
	}
	d, err := stage.Sub("original-runtime")
	if err != nil {
		return "", err
	}
	defer d.Close()
	budget := recoveryArchiveBudget{limit: c.MaxDatabaseBytes, reserve: c.MinFreeBytes}
	for _, f := range files {
		if err = evidenceFileMatches(source, f.Path, f); err != nil {
			return "", err
		}
		if parent := path.Dir(f.Path); parent != "." {
			if err = d.Mkdir(parent); err != nil {
				return "", err
			}
		}
		if err = copyRecoveryProofFile(ctx, source, d, f.Path, &budget); err != nil {
			return "", err
		}
	}
	if err = stage.WriteJSON("original-runtime.json", files, false); err != nil {
		return "", err
	}
	return protocol.Hash(files), nil
}

func verifyOriginalRuntime(ctx context.Context, stage *safefs.Directory, j snapshotRecovery, c Config) error {
	if j.Version == 1 {
		return nil
	}
	var files []recoveryEvidenceFile
	if err := readCheckpointJSON(stage, "original-runtime.json", &files); err != nil {
		return err
	}
	if protocol.Hash(files) != j.RuntimeHash {
		return fmt.Errorf("original runtime audit inventory changed")
	}
	if err := validateSnapshotRuntimeInventory(files, c); err != nil {
		return err
	}
	d, err := stage.Sub("original-runtime")
	if err != nil {
		return err
	}
	defer d.Close()
	for _, f := range files {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = evidenceFileMatches(d, f.Path, f); err != nil {
			return err
		}
	}
	return nil
}
