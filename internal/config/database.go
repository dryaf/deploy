package config

import (
	"fmt"
	"path"
	"path/filepath"
)

func DatabasePaths(env Environment) (local, remote string, err error) {
	if env.Database.Driver != "sqlite" {
		return "", "", fmt.Errorf("only sqlite databases are supported")
	}
	if err := ValidateArtifactPath(env.Database.Source); err != nil {
		return "", "", fmt.Errorf("database source: %w", err)
	}
	return filepath.Clean(env.Database.Source), path.Join(env.Dir, env.Database.Source), nil
}
