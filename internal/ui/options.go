package ui

import (
	"path/filepath"
)

// --- Global Flags ---
var (
	DryRun      bool
	Verbose     bool
	ServersFile string
)

func InventoryPath(application string) string {
	if ServersFile != "" {
		return ServersFile
	}
	if application != "" {
		return filepath.Join(filepath.Dir(application), "server.yaml")
	}
	return "server.yaml"
}
