// Package ledartifact validates the bounded, offline pi-status-led zipapp used
// by managed provisioning. Artifact inspection never executes the archive.
package ledartifact

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/dryaf/deploy/internal/safefs"
)

const SourceCommit = "fbe71f9f6edd108beceb01d568a0b17b5a6eb85c"
const MaxBytes = 8 << 20

//go:embed launcher.py
var launcher []byte

//go:embed removal_guard.py
var removalGuard []byte

type Metadata struct {
	Version       int               `json:"version"`
	DaemonVersion string            `json:"daemon_version"`
	SourceCommit  string            `json:"source_commit"`
	RestoreGuard  int               `json:"restore_guard"`
	RemovalGuard  int               `json:"removal_guard"`
	Files         map[string]string `json:"files"`
}

func Verify(file, digest string) (Metadata, error) {
	var zero Metadata
	if !safefs.DigestPattern.MatchString(digest) {
		return zero, fmt.Errorf("LED artifact requires an exact SHA256")
	}
	f, err := os.Open(file)
	if err != nil {
		return zero, fmt.Errorf("LED artifact unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxBytes {
		return zero, fmt.Errorf("LED artifact must be a bounded regular archive")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil || len(raw) > MaxBytes || hash(raw) != digest {
		return zero, fmt.Errorf("LED artifact digest changed")
	}
	return Inspect(raw)
}

func Inspect(raw []byte) (Metadata, error) {
	var metadata Metadata
	if len(raw) == 0 || len(raw) > MaxBytes {
		return metadata, fmt.Errorf("LED archive exceeds bounds")
	}
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || len(z.File) < 5 || len(z.File) > 64 {
		return metadata, fmt.Errorf("invalid LED archive")
	}
	files := map[string][]byte{}
	total := 0
	for _, f := range z.File {
		name := f.Name
		allowed := name == "__main__.py" || name == "LICENSE" || name == "deploy-led-artifact.json" || strings.HasPrefix(name, "pistatus/") && path.Dir(name) == "pistatus" && strings.HasSuffix(name, ".py")
		if !allowed || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00\r\n") || f.Mode()&os.ModeType != 0 || f.UncompressedSize64 > 512<<10 || files[name] != nil {
			return metadata, fmt.Errorf("unsafe LED archive entry")
		}
		r, e := f.Open()
		if e != nil {
			return metadata, e
		}
		body, e := io.ReadAll(io.LimitReader(r, 512<<10+1))
		closeErr := r.Close()
		if e != nil || closeErr != nil || len(body) > 512<<10 {
			return metadata, fmt.Errorf("LED archive entry unreadable")
		}
		total += len(body)
		if total > MaxBytes {
			return metadata, fmt.Errorf("LED archive expansion exceeds budget")
		}
		files[name] = body
	}
	if !bytes.Equal(files["pistatus/deploy_removal.py"], removalGuard) || !bytes.Equal(files["__main__.py"], launcher) || len(files["LICENSE"]) == 0 || len(files["pistatus/daemon.py"]) == 0 || len(files["pistatus/sysfs.py"]) == 0 || len(files["pistatus/__init__.py"]) == 0 {
		return metadata, fmt.Errorf("LED artifact lacks required runtime or restore guard")
	}
	if err = safefs.DecodeJSON(files["deploy-led-artifact.json"], &metadata, 32<<10); err != nil {
		return metadata, fmt.Errorf("invalid LED artifact metadata")
	}
	if metadata.Version != 1 || metadata.DaemonVersion != "0.1.0" || metadata.SourceCommit != SourceCommit || metadata.RestoreGuard != 1 || metadata.RemovalGuard != 1 || len(metadata.Files) != len(files)-1 {
		return metadata, fmt.Errorf("unsupported LED artifact release")
	}
	for name, body := range files {
		if name != "deploy-led-artifact.json" && metadata.Files[name] != hash(body) {
			return metadata, fmt.Errorf("LED artifact contents differ from metadata")
		}
	}
	return metadata, nil
}

func hash(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
