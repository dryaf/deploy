package deployment

import (
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
)

type backupRecord struct {
	Schema      int    `json:"schema"`
	Database    string `json:"database"`
	Snapshot    string `json:"snapshot"`
	Environment string `json:"environment"`
	Operation   string `json:"operation"`
	SHA256      string `json:"sha256"`
	CreatedAt   string `json:"created_at"`
}

func recordLocalBackup(database, snapshot, environment, operation string) error {
	checksum, err := safefs.FileChecksum(snapshot)
	if err != nil {
		return err
	}
	database, err = filepath.Abs(database)
	if err != nil {
		return err
	}
	return safefs.WritePrivateJSON(snapshot+".meta.json", backupRecord{Schema: 1, Database: database, Snapshot: filepath.Base(snapshot), Environment: environment, Operation: operation, SHA256: checksum, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}
