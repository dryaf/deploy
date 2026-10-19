package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func pruneLocalDatabase(e Environment, preview bool) error {
	database, _, err := databasePaths(e)
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Dir(database)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	lock, err := acquireLocalLock("database", database)
	if err != nil {
		return err
	}
	defer lock.Close()
	dir := filepath.Join(filepath.Dir(database), "backup")
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("backup must be a private real directory")
	}
	abs, err := filepath.Abs(database)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type entry struct {
		file, metadata string
		created        time.Time
		record         backupRecord
	}
	var inventory []entry
	for _, item := range entries {
		if !strings.HasSuffix(item.Name(), ".meta.json") {
			continue
		}
		metadata := filepath.Join(dir, item.Name())
		info, err := os.Lstat(metadata)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe backup metadata: %s", metadata)
		}
		body, err := os.ReadFile(metadata)
		if err != nil {
			return err
		}
		var record backupRecord
		if err = json.Unmarshal(body, &record); err != nil || record.Schema != 1 {
			return fmt.Errorf("corrupt backup inventory: %s", metadata)
		}
		if record.Database != abs || record.Environment != "local" || record.Operation != "before-pull-from-"+e.Name {
			continue
		}
		if record.Snapshot != strings.TrimSuffix(item.Name(), ".meta.json") || filepath.Base(record.Snapshot) != record.Snapshot {
			return fmt.Errorf("unsafe backup inventory path")
		}
		file := filepath.Join(dir, record.Snapshot)
		info, err = os.Lstat(file)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe backup snapshot: %s", file)
		}
		created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
		if err != nil {
			return fmt.Errorf("invalid backup creation timestamp: %s", metadata)
		}
		inventory = append(inventory, entry{file: file, metadata: metadata, created: created, record: record})
	}
	sort.Slice(inventory, func(i, j int) bool {
		if inventory[i].created.Equal(inventory[j].created) {
			return inventory[i].file > inventory[j].file
		}
		return inventory[i].created.After(inventory[j].created)
	})
	policy := retentionPolicyFor(e.Database.Retention)
	var candidates []entry
	for index, item := range inventory {
		expired := policy.Enabled && index >= policy.Keep && (policy.AgeSeconds == 0 || time.Since(item.created).Seconds() > float64(policy.AgeSeconds))
		if !expired {
			fmt.Printf("KEEP within retention %s\n", item.file)
			continue
		}
		checksum, err := fileChecksum(item.file)
		if err != nil {
			return err
		}
		if checksum != item.record.SHA256 {
			return fmt.Errorf("backup checksum mismatch; cleanup refused: %s", item.file)
		}
		fmt.Printf("DELETE %s\n", item.file)
		candidates = append(candidates, item)
	}
	if preview {
		return nil
	}
	for _, item := range candidates {
		// A crash between unlinks leaves an untracked snapshot which future cleanup
		// retains, instead of metadata referring to a missing snapshot.
		if err = os.Remove(item.metadata); err != nil {
			return err
		}
		if err = syncFile(dir); err != nil {
			return err
		}
		if err = os.Remove(item.file); err != nil {
			return err
		}
		if err = syncFile(dir); err != nil {
			return err
		}
	}
	return nil
}
