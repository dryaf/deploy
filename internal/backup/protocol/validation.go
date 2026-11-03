package protocol

import (
	"time"
)

type SnapshotValidation struct {
	ForeignKeys   string    `json:"foreign_keys,omitempty"`
	Check         string    `json:"check"`
	Result        string    `json:"result"`
	SQLiteVersion string    `json:"sqlite_version"`
	CheckedAt     time.Time `json:"checked_at"`
}
