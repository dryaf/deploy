package source

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
)

func numericContainerUser(value string) (int, int, error) {
	if value == "" || value == "0" {
		return 0, 0, nil
	}
	uidText, gidText, hasGroup := strings.Cut(value, ":")
	if !hasGroup {
		return 0, 0, fmt.Errorf("backup profile requires explicit numeric UID:GID for non-root OCI users")
	}
	uid, err := strconv.Atoi(uidText)
	if err != nil || uid < 0 {
		return 0, 0, fmt.Errorf("unsupported OCI user")
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil || gid < 0 {
		return 0, 0, fmt.Errorf("unsupported OCI group")
	}
	return uid, gid, nil
}

func verifySourceRuntimeBinding(home string, m quadlet.QuadletMetadata, e config.Environment, release, containerID string) error {
	var manifest struct {
		ImageID string `json:"image_id"`
	}
	if err := readSourceJSON(filepath.Join(m.TargetDir, ".deploy", string(m.Mode), "releases", release, "manifest.json"), &manifest); err != nil {
		return err
	}
	imageID := strings.TrimPrefix(manifest.ImageID, "sha256:")
	if !safefs.DigestPattern.MatchString(imageID) {
		return fmt.Errorf("committed image identity is unavailable")
	}
	template := `{"id":{{json .ID}},"user":{{json .Config.User}}}`
	cmd := exec.Command("/usr/bin/podman", "image", "inspect", "--format", template, manifest.ImageID)
	cmd.Env = SourceProcessEnvironment(home)
	output, err := process.LimitedOutput(cmd, 5*time.Second, 4096)
	if err != nil {
		return fmt.Errorf("committed image configuration unavailable")
	}
	var image struct {
		ID   string `json:"id"`
		User string `json:"user"`
	}
	if err = protocol.StrictBackupJSON(output, &image); err != nil || strings.TrimPrefix(image.ID, "sha256:") != imageID {
		return fmt.Errorf("committed image configuration mismatch")
	}
	uid, gid, err := numericContainerUser(image.User)
	if err != nil {
		return err
	}
	if uid != m.Backup.ContainerUID || gid != m.Backup.ContainerGID {
		return fmt.Errorf("committed OCI user differs from declared database UID/GID")
	}
	if containerID == "" {
		return nil
	}
	if !safefs.DigestPattern.MatchString(containerID) {
		return fmt.Errorf("effective container identity is not canonical")
	}
	template = `{"image":{{json .Image}},"user":{{json .Config.User}},"namespace":{{json .HostConfig.UsernsMode}},"mounts":{{json .Mounts}},"release":{{json (index .Config.Labels "io.deploy.release")}},"service":{{json (index .Config.Labels "io.deploy.service")}}}`
	cmd = exec.Command("/usr/bin/podman", "inspect", "--format", template, containerID)
	cmd.Env = SourceProcessEnvironment(home)
	output, err = process.LimitedOutput(cmd, 5*time.Second, 64<<10)
	if err != nil {
		return fmt.Errorf("effective container inspection unavailable")
	}
	var runtime struct {
		Image     string `json:"image"`
		User      string `json:"user"`
		Namespace string `json:"namespace"`
		Mounts    []struct {
			Type        string
			Source      string
			Destination string
		} `json:"mounts"`
		Release string `json:"release"`
		Service string `json:"service"`
	}
	// Mount objects contain additive Podman fields. Project only the fields the
	// binding comparison needs; never request environment or command contents.
	if err = safefs.RejectDuplicateJSONKeys(output); err != nil {
		return err
	}
	if err = json.Unmarshal(output, &runtime); err != nil {
		return err
	}
	if strings.TrimPrefix(runtime.Image, "sha256:") != imageID || runtime.Release != release || runtime.Service != m.Service {
		return fmt.Errorf("effective container is not the committed release")
	}
	if runtime.Namespace != "" && runtime.Namespace != "host" {
		return fmt.Errorf("unsupported user namespace mapping")
	}
	uid, gid, err = numericContainerUser(runtime.User)
	if err != nil || uid != m.Backup.ContainerUID || gid != m.Backup.ContainerGID {
		return fmt.Errorf("effective container UID/GID differs from metadata")
	}
	databaseDir := filepath.Join(m.TargetDir, filepath.Dir(m.Backup.Source))
	destination := ""
	for _, volume := range e.Quadlet.Volumes {
		parts := strings.Split(volume, ":")
		if len(parts) >= 2 && filepath.Clean(parts[0]) == databaseDir {
			destination = filepath.Clean(parts[1])
		}
	}
	matches := 0
	for _, mount := range runtime.Mounts {
		if config.PathsOverlap(filepath.Clean(mount.Destination), destination) {
			if mount.Type != "bind" || filepath.Clean(mount.Source) != databaseDir || filepath.Clean(mount.Destination) != destination {
				return fmt.Errorf("effective database bind differs from metadata")
			}
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("effective database bind is missing or ambiguous")
	}
	return nil
}
