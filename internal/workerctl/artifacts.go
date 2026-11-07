package workerctl

import (
	"crypto/sha256"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/ledartifact"
	"github.com/dryaf/deploy/internal/safefs"
)

type Artifact struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}
type ArchitectureArtifacts struct {
	Helper      Artifact  `json:"helper"`
	BackupImage string    `json:"backup_image,omitempty"`
	RelayImage  string    `json:"relay_image,omitempty"`
	Signal      *Artifact `json:"signal,omitempty"`
	LEDs        *Artifact `json:"leds,omitempty"`
}
type Catalog struct {
	Version int                              `json:"version"`
	Release string                           `json:"release"`
	Linux   map[string]ArchitectureArtifacts `json:"linux"`
}

type ResolvedArtifacts struct {
	Release    string    `json:"release"`
	Helper     Artifact  `json:"helper"`
	Image      string    `json:"image,omitempty"`
	RelayImage string    `json:"relay_image,omitempty"`
	Signal     *Artifact `json:"signal,omitempty"`
	LEDs       *Artifact `json:"leds,omitempty"`
}

func ReadCatalog(file string) (Catalog, error) {
	var c Catalog
	f, err := os.Open(file)
	if err != nil {
		return c, fmt.Errorf("artifact catalog unavailable; build or install a worker bundle or set artifacts.catalog")
	}
	defer f.Close()
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return c, fmt.Errorf("artifact catalog must be a bounded regular file")
	}
	if err = safefs.ReadJSON(f, &c, 1<<20); err != nil {
		return c, fmt.Errorf("invalid artifact catalog")
	}
	if c.Version != 1 || !config.ValidInventoryName(c.Release) || len(c.Linux) == 0 {
		return c, fmt.Errorf("invalid artifact catalog version or release")
	}
	for arch, a := range c.Linux {
		if arch != "amd64" && arch != "arm64" {
			return c, fmt.Errorf("unsupported catalog architecture")
		}
		for _, v := range []*Artifact{&a.Helper, a.Signal, a.LEDs} {
			if v == nil {
				continue
			}
			if filepath.IsAbs(v.File) || filepath.Clean(v.File) != v.File || v.File == "." || v.File == ".." || strings.HasPrefix(v.File, "../") || !validDigest(v.SHA256) {
				return c, fmt.Errorf("catalog artifact requires a relative path and SHA256")
			}
			v.File = filepath.Join(filepath.Dir(file), v.File)
		}
		for _, v := range []string{a.BackupImage, a.RelayImage} {
			if v != "" {
				if err = config.PinnedWorkerImage(v); err != nil {
					return c, err
				}
			}
		}
		c.Linux[arch] = a
	}
	return c, nil
}

func ResolveArtifacts(declared config.WorkerArtifacts, arch, defaultCatalog string) (ResolvedArtifacts, error) {
	var r ResolvedArtifacts
	file := declared.Catalog
	if file == "" {
		file = defaultCatalog
	}
	if file != "" {
		c, err := ReadCatalog(file)
		if err != nil {
			return r, err
		}
		a, ok := c.Linux[arch]
		if !ok {
			return r, fmt.Errorf("catalog has no matching target architecture")
		}
		r = ResolvedArtifacts{Release: c.Release, Helper: a.Helper, Image: a.BackupImage, RelayImage: a.RelayImage, Signal: a.Signal, LEDs: a.LEDs}
	}
	if declared.Helper != "" {
		r.Helper = Artifact{declared.Helper, declared.HelperSHA256}
	}
	if declared.Image != "" {
		r.Image = declared.Image
	}
	if declared.RelayImage != "" {
		r.RelayImage = declared.RelayImage
	}
	if r.Helper.File == "" {
		return r, fmt.Errorf("matching helper unavailable; install a worker artifact bundle or set artifacts.catalog")
	}
	if err := VerifyArtifact(r.Helper, arch, true); err != nil {
		return r, err
	}
	return r, nil
}

func VerifyArtifact(a Artifact, arch string, static bool) error {
	return verifyArtifact(a, arch, static, static, 128<<20)
}

// VerifySignalArtifact accepts a pinned native Linux CLI for the target. Unlike
// deploy helpers, the supported native CLI need not be statically linked.
func VerifySignalArtifact(a Artifact, arch string) error {
	return verifyArtifact(a, arch, true, false, 512<<20)
}

func ResolveLEDArtifact(d config.LEDDaemon, catalog *Artifact) (*Artifact, error) {
	if d.Mode != "managed" {
		return nil, fmt.Errorf("LED artifacts belong to managed daemon mode")
	}
	a := catalog
	if d.Artifact != "" {
		a = &Artifact{File: d.Artifact, SHA256: d.SHA256}
	}
	if a == nil {
		return nil, fmt.Errorf("worker bundle requires a pinned LED daemon archive; build scripts/build-led-artifact.py or select status_leds.daemon.artifact and sha256")
	}
	if _, err := ledartifact.Verify(a.File, a.SHA256); err != nil {
		return nil, err
	}
	copy := *a
	return &copy, nil
}
func verifyArtifact(a Artifact, arch string, native, static bool, limit int64) error {
	if !validDigest(a.SHA256) {
		return fmt.Errorf("artifact digest must be SHA256")
	}
	f, err := os.Open(a.File)
	if err != nil {
		return fmt.Errorf("artifact file unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > limit {
		return fmt.Errorf("artifact must be a bounded regular file")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, limit+1)); err != nil {
		return fmt.Errorf("artifact read failed")
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("artifact digest mismatch")
	}
	if !native {
		return nil
	}
	binary, err := elf.NewFile(f)
	if err != nil {
		return fmt.Errorf("artifact must be a Linux ELF executable")
	}
	defer binary.Close()
	expected := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if expected == 0 || binary.Machine != expected || binary.Class != elf.ELFCLASS64 || binary.Data != elf.ELFDATA2LSB || (binary.Type != elf.ET_EXEC && binary.Type != elf.ET_DYN) {
		return fmt.Errorf("artifact architecture mismatch")
	}
	executableEntry := false
	for _, p := range binary.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 && binary.Entry >= p.Vaddr && binary.Entry-p.Vaddr < p.Filesz {
			executableEntry = true
		}
		if static && p.Type == elf.PT_INTERP {
			return fmt.Errorf("helper must be statically linked")
		}
	}
	if !executableEntry {
		return fmt.Errorf("artifact has no executable entry point")
	}
	return nil
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
