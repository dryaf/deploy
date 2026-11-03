package backup

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
)

func backupEvaluationMeta(tx *sql.Tx, key string, dest any) error {
	var raw string
	if err := tx.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&raw); err != nil {
		return err
	}
	return safefs.DecodeJSON([]byte(raw), dest, safefs.MaxRecordBytes)
}

// Read the catalogue and its negative verification evidence in the caller's
// transaction. The separately refreshed host probe establishes file availability.
func backupSnapshotEvidence(tx *sql.Tx, control ArchiveControl, instance ExpectedInstance) (receipt SnapshotReceipt, problem string, err error) {
	var raw string
	err = tx.QueryRow(`SELECT record FROM bundles WHERE json_extract(record,'$.header.attempt.source_id')=? AND json_extract(record,'$.header.attempt.service')=? AND json_extract(record,'$.header.generation')=? ORDER BY json_extract(record,'$.writer_epoch') DESC,json_extract(record,'$.header.attempt.capture_sequence') DESC LIMIT 1`, instance.SourceID, instance.Service, instance.Generation).Scan(&raw)
	if err == sql.ErrNoRows {
		return receipt, "latest snapshot is unavailable", nil
	}
	if err != nil {
		return
	}
	if err = safefs.DecodeJSON([]byte(raw), &receipt, safefs.MaxRecordBytes); err != nil {
		return
	}
	if receipt.Schema != 1 || receipt.BackupID == "" || receipt.Header.Attempt.RequestedAt.IsZero() || !safefs.DigestPattern.MatchString(receipt.Header.SHA256) || receipt.ArchiveID != control.ArchiveID || receipt.Header.Attempt.BindingHash != instance.BindingHash || receipt.Validation.Result != "ok" {
		return receipt, "latest snapshot identity or validation is uncertain", nil
	}
	latest, known := control.LatestSnapshots[latestSnapshotKey(receipt)]
	if !known || latest.BackupID != receipt.BackupID || latest.WriterEpoch != receipt.WriterEpoch || latest.CaptureSequence != receipt.Header.Attempt.CaptureSequence {
		return receipt, "latest publication is missing or inconsistent", nil
	}
	var unavailable string
	if err = backupEvaluationMeta(tx, "unavailable/"+receipt.BackupID, &unavailable); err != nil && err != sql.ErrNoRows {
		return
	}
	if unavailable != "" {
		return receipt, "latest snapshot is unavailable", nil
	}
	var status BackupVerificationStatus
	err = backupEvaluationMeta(tx, "verification/"+receipt.BackupID, &status)
	if err == sql.ErrNoRows {
		return receipt, "", nil
	}
	if err != nil {
		return
	}
	for _, record := range []*ArchiveVerification{&status.Last, status.Definitive} {
		if record != nil && (record.BackupID != receipt.BackupID || record.ArchiveID != receipt.ArchiveID || record.SHA256 != receipt.Header.SHA256) {
			return receipt, "verification identity conflict", nil
		}
	}
	if status.Definitive != nil && status.Definitive.ErrorKind != "" {
		return receipt, "known-corrupt latest snapshot (" + status.Definitive.ErrorKind + ")", nil
	}
	if status.Last.ErrorKind != "" || status.Last.Validation.Result != "ok" {
		return receipt, "latest verification is inconclusive", nil
	}
	return receipt, "", nil
}

type backupEvaluationClock struct {
	Now    time.Time
	Boot   string
	Uptime float64
}

func (c backupEvaluationClock) valid() bool {
	return !c.Now.IsZero() && c.Boot != "" && c.Uptime >= 0 && !math.IsNaN(c.Uptime) && !math.IsInf(c.Uptime, 0)
}

// Clamp both wall and monotonic lifetimes. A refreshed publication cannot extend
// the stored probe's original 120-second lease.
func backupProbeDeadline(p WorkerHealthProbe, c backupEvaluationClock, hash string) (time.Time, bool) {
	elapsed := c.Now.Sub(p.ObservedAt).Seconds()
	monotonic := c.Uptime - p.Uptime
	if !c.valid() || p.Schema != 1 || p.BootID != c.Boot || p.ConfigHash != hash || p.ObservedAt.IsZero() || p.Uptime < 0 || math.IsNaN(p.Uptime) || math.IsInf(p.Uptime, 0) || elapsed < 0 || elapsed >= 120 || monotonic < 0 || monotonic >= 120 || absFloat(elapsed-monotonic) >= 30 {
		return c.Now, false
	}
	deadline := p.ObservedAt.Add(120 * time.Second)
	monoDeadline := c.Now.Add(time.Duration((120 - monotonic) * float64(time.Second)))
	if monoDeadline.Before(deadline) {
		deadline = monoDeadline
	}
	return deadline, true
}

func backupEvaluationDurations(freshness, run, repeat string) (time.Duration, time.Duration, error) {
	values := []string{freshness, run, repeat}
	for _, v := range values {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return 0, 0, fmt.Errorf("validated positive backup evaluation durations required")
		}
	}
	fresh, _ := time.ParseDuration(freshness)
	limit, _ := time.ParseDuration(run)
	return fresh, limit, nil
}
