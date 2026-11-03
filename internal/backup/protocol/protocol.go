package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
)

const BackupProtocol = 1

const backupWireMagic = "DPLBK001"

const MaxBackupHeader = 64 << 10

type SnapshotAttempt struct {
	Protocol        int       `json:"protocol"`
	Operation       string    `json:"operation"`
	Nonce           string    `json:"nonce"`
	RunID           string    `json:"run_id"`
	ConfigHash      string    `json:"config_hash"`
	CaptureSequence uint64    `json:"capture_sequence"`
	SourceID        string    `json:"source_id"`
	Service         string    `json:"service"`
	Generation      string    `json:"generation"`
	BindingHash     string    `json:"binding_hash"`
	RequestedAt     time.Time `json:"requested_at"`
}

func (a SnapshotAttempt) Validate() error {
	if a.Protocol != BackupProtocol || a.Operation != "snapshot" || !safefs.ValidID(a.Nonce, "n") || !safefs.ValidID(a.RunID, "r") || !safefs.ValidID(a.SourceID, "s") || !config.ServicePattern.MatchString(a.Service) || !safefs.DigestPattern.MatchString(a.BindingHash) || !safefs.DigestPattern.MatchString(a.ConfigHash) || a.CaptureSequence == 0 || a.RequestedAt.IsZero() {
		return fmt.Errorf("invalid snapshot request")
	}
	if a.Generation != "unenrolled" && !safefs.ValidID(a.Generation, "g") {
		return fmt.Errorf("invalid database generation")
	}
	return nil
}

type SnapshotHeader struct {
	Protocol      int                     `json:"protocol"`
	Role          string                  `json:"role"`
	Attempt       SnapshotAttempt         `json:"attempt"`
	SnapshotID    string                  `json:"snapshot_id"`
	Generation    string                  `json:"generation"`
	Metadata      quadlet.QuadletMetadata `json:"metadata"`
	ReleaseID     string                  `json:"release_id"`
	StartedAt     time.Time               `json:"started_at"`
	FinishedAt    time.Time               `json:"finished_at"`
	Size          int64                   `json:"size"`
	SHA256        string                  `json:"sha256"`
	Format        string                  `json:"format"`
	SQLiteVersion string                  `json:"sqlite_version"`
}

type SnapshotFooter struct {
	Complete     bool   `json:"complete"`
	HeaderSHA256 string `json:"header_sha256"`
	SHA256       string `json:"sha256"`
}

func StrictBackupJSON(data []byte, dest any) error {
	return safefs.DecodeJSON(data, dest, MaxBackupHeader)
}

func WriteBackupFrame(w io.Writer, data []byte) error {
	if len(data) == 0 || len(data) > MaxBackupHeader {
		return fmt.Errorf("invalid protocol frame length")
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func ReadBackupFrame(r io.Reader) ([]byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n == 0 || n > MaxBackupHeader {
		return nil, fmt.Errorf("protocol frame exceeds limit")
	}
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return body, err
}

func ValidateSnapshotHeader(h SnapshotHeader, expected SnapshotAttempt, maxBytes int64) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	// A response is bound to every immutable field of the fresh request.
	a, _ := json.Marshal(h.Attempt)
	b, _ := json.Marshal(expected)
	if !bytes.Equal(a, b) || h.Protocol != BackupProtocol || h.Role != "source-snapshot" {
		return fmt.Errorf("snapshot response does not match the current attempt")
	}
	if !safefs.ValidID(h.SnapshotID, "b") || !safefs.ValidID(h.Generation, "g") || (expected.Generation != "unenrolled" && h.Generation != expected.Generation) || h.Size < 512 || h.Size > maxBytes || !safefs.DigestPattern.MatchString(h.SHA256) || h.Format != "sqlite-delete-v1" || h.StartedAt.IsZero() || h.FinishedAt.Before(h.StartedAt) || h.ReleaseID == "" {
		return fmt.Errorf("invalid snapshot identity, format, size or capture interval")
	}
	if err := quadlet.ValidateMetadata(h.Metadata); err != nil {
		return err
	}
	if h.Metadata.Service != expected.Service || h.Metadata.Role != "application" || h.Metadata.Backup == nil || !h.Metadata.Backup.Enabled {
		return fmt.Errorf("snapshot metadata is not authorized application opt-in")
	}
	if MetadataBindingHash(h.Metadata) != expected.BindingHash {
		return fmt.Errorf("snapshot database binding changed")
	}
	return nil
}

func MetadataBindingHash(m quadlet.QuadletMetadata) string {
	// No release, runtime ID, uptime or observation time enters this identity.
	body, _ := json.Marshal(m)
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

func WriteSnapshotTransfer(w io.Writer, h SnapshotHeader, body io.Reader) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(w, backupWireMagic); err != nil {
		return err
	}
	if err = WriteBackupFrame(w, data); err != nil {
		return err
	}
	digest := sha256.New()
	n, err := io.CopyN(io.MultiWriter(w, digest), body, h.Size)
	if err != nil || n != h.Size {
		return fmt.Errorf("incomplete snapshot body: %w", err)
	}
	var extra [1]byte
	if n, err := body.Read(extra[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("snapshot exceeds declared size")
	}
	if fmt.Sprintf("%x", digest.Sum(nil)) != h.SHA256 {
		return fmt.Errorf("snapshot changed before transfer completion")
	}
	footer, _ := json.Marshal(SnapshotFooter{true, fmt.Sprintf("%x", sha256.Sum256(data)), h.SHA256})
	return WriteBackupFrame(w, footer)
}

func ReadSnapshotTransfer(r io.Reader, w io.Writer, attempt SnapshotAttempt, maxBytes int64) (SnapshotHeader, error) {
	return ReadSnapshotTransferPhased(r, w, attempt, maxBytes, nil)
}

func ReadSnapshotTransferPhased(r io.Reader, w io.Writer, attempt SnapshotAttempt, maxBytes int64, onHeader func() error) (SnapshotHeader, error) {
	var h SnapshotHeader
	magic := make([]byte, len(backupWireMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return h, err
	}
	if string(magic) != backupWireMagic {
		return h, fmt.Errorf("invalid backup protocol magic")
	}
	data, err := ReadBackupFrame(r)
	if err != nil {
		return h, err
	}
	if err = StrictBackupJSON(data, &h); err != nil {
		return h, err
	}
	if err = ValidateSnapshotHeader(h, attempt, maxBytes); err != nil {
		return h, err
	}
	if onHeader != nil {
		if err = onHeader(); err != nil {
			return h, err
		}
	}
	hash := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(w, hash), r, h.Size); err != nil {
		return h, fmt.Errorf("truncated backup stream: %w", err)
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != h.SHA256 {
		return h, fmt.Errorf("snapshot checksum mismatch")
	}
	footerBytes, err := ReadBackupFrame(r)
	if err != nil {
		return h, err
	}
	var footer SnapshotFooter
	if err = StrictBackupJSON(footerBytes, &footer); err != nil {
		return h, err
	}
	if !footer.Complete || footer.SHA256 != h.SHA256 || footer.HeaderSHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
		return h, fmt.Errorf("invalid snapshot completion footer")
	}
	var trailing [1]byte
	if n, err := r.Read(trailing[:]); n != 0 || err != io.EOF {
		return h, fmt.Errorf("unexpected bytes or incomplete protocol exit")
	}
	return h, nil
}
