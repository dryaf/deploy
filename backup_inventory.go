package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
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

func fileChecksum(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func writePrivateJSON(destination string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(destination), ".deploy-json-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(value); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), destination); err != nil {
		return err
	}
	return syncFile(filepath.Dir(destination))
}

func recordLocalBackup(database, snapshot, environment, operation string) error {
	checksum, err := fileChecksum(snapshot)
	if err != nil {
		return err
	}
	database, err = filepath.Abs(database)
	if err != nil {
		return err
	}
	return writePrivateJSON(snapshot+".meta.json", backupRecord{Schema: 1, Database: database, Snapshot: filepath.Base(snapshot), Environment: environment, Operation: operation, SHA256: checksum, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}
