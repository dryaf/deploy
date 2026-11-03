package quadlet

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

const MetadataPrefix = "# deploy:metadata "

const maxMetadataBytes = 16 << 10

const MaxQuadletBytes = 1 << 20

var ErrUnsupportedMetadata = errors.New("unsupported metadata schema")

// Public operational claims, never credentials or authorization evidence.
type QuadletMetadata struct {
	Schema      int                    `json:"schema"`
	ManagedBy   string                 `json:"managed_by"`
	Role        string                 `json:"role"`
	App         string                 `json:"app,omitempty"`
	Environment string                 `json:"environment,omitempty"`
	Service     string                 `json:"service"`
	Mode        config.DeploymentMode  `json:"mode,omitempty"`
	TargetDir   string                 `json:"target_dir,omitempty"`
	Backup      *QuadletBackupMetadata `json:"backup,omitempty"`
}

type QuadletBackupMetadata struct {
	Enabled      bool   `json:"enabled"`
	Driver       string `json:"driver,omitempty"`
	Source       string `json:"source,omitempty"`
	ContainerUID int    `json:"container_uid"`
	ContainerGID int    `json:"container_gid"`
}

func applicationMetadata(n config.NormalizedConfig) QuadletMetadata {
	e := n.Env
	return QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: "application", App: n.Config.AppName, Environment: e.Name, Service: e.Quadlet.ServiceName, Mode: n.Mode, TargetDir: e.Dir, Backup: &QuadletBackupMetadata{e.Database.Backup.Enabled, e.Database.Driver, e.Database.Source, e.Quadlet.ContainerUID, e.Quadlet.ContainerGID}}
}

func EncodeMetadata(m QuadletMetadata) (string, error) {
	if err := ValidateMetadata(m); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if len(data)+len(MetadataPrefix) > maxMetadataBytes {
		return "", fmt.Errorf("metadata exceeds 16 KiB")
	}
	return MetadataPrefix + string(data) + "\n", nil
}

func ValidateMetadata(m QuadletMetadata) error {
	if m.Schema != 1 {
		return ErrUnsupportedMetadata
	}
	if m.ManagedBy != "deploy" || !config.ServicePattern.MatchString(m.Service) || strings.Contains(m.Service, "..") {
		return fmt.Errorf("invalid metadata management or service identity")
	}
	for _, s := range []string{m.App, m.Environment, m.TargetDir, m.Service, m.Role, string(m.Mode)} {
		if !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
			return fmt.Errorf("metadata identity contains control characters or invalid UTF-8")
		}
	}
	switch m.Role {
	case "application":
		if m.App == "" || m.Environment == "" || !path.IsAbs(m.TargetDir) || path.Clean(m.TargetDir) != m.TargetDir || m.TargetDir == "/" || (m.Mode != config.ModeBinary && m.Mode != config.ModeImage) {
			return fmt.Errorf("incomplete application metadata")
		}
		if m.Backup == nil {
			return fmt.Errorf("application metadata requires explicit backup policy")
		}
		if m.Backup.ContainerUID < 0 || m.Backup.ContainerGID < 0 {
			return fmt.Errorf("invalid metadata container identity")
		}
		if m.Backup.Enabled {
			if m.Backup.Driver != "sqlite" {
				return fmt.Errorf("backup metadata requires SQLite")
			}
			if err := config.ValidateArtifactPath(m.Backup.Source); err != nil {
				return err
			}
			if strings.IndexFunc(m.Backup.Source, unicode.IsControl) >= 0 {
				return fmt.Errorf("invalid backup source")
			}
		}
	case "infrastructure", "maintenance", "backup-worker", "notification-relay":
		if m.Backup != nil || m.Mode != "" || m.TargetDir != "" {
			return fmt.Errorf("non-application metadata cannot declare a database or target")
		}
	default:
		return fmt.Errorf("unknown metadata role")
	}
	return nil
}

// Unknown additive fields are inert. Duplicate keys at any depth are rejected,
// so different parsers cannot disagree about fields relevant to authorization.
func ParseMetadata(r io.Reader) (*QuadletMetadata, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxQuadletBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxQuadletBytes {
		return nil, fmt.Errorf("Quadlet exceeds 1 MiB")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), MaxQuadletBytes+1)
	var raw []byte
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "# deploy:metadata") {
			continue
		}
		if raw != nil {
			return nil, fmt.Errorf("duplicate metadata markers")
		}
		if !strings.HasPrefix(line, MetadataPrefix) || len(scanner.Bytes()) > maxMetadataBytes {
			return nil, fmt.Errorf("malformed or oversized metadata marker")
		}
		raw = []byte(strings.TrimPrefix(line, MetadataPrefix))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("metadata is not UTF-8")
	}
	if err := safefs.RejectDuplicateJSONKeys(raw); err != nil {
		return nil, err
	}
	var m QuadletMetadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid metadata JSON")
	}
	if err := ValidateMetadata(m); err != nil {
		return &m, err
	}
	return &m, nil
}

func (d TraefikTemplateData) Metadata() (string, error) {
	return EncodeMetadata(QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: "infrastructure", Service: "traefik"})
}

func (d MaintenanceTemplateData) Metadata() (string, error) {
	return EncodeMetadata(QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: "maintenance", Service: d.ServiceName + "-maint"})
}
