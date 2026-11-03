package source

import (
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

const SourceRequestsPerHour = 1024

const sourceCaptureSecondsPerHour = 1800

const SourceExportBytesPerHour = 6 * sourceByteCap

type SourceRateUsage struct {
	Requests       int     `json:"requests"`
	CaptureSeconds float64 `json:"capture_seconds"`
	ExportBytes    int64   `json:"export_bytes"`
}

type SourceRateLedger struct {
	Schema       int                        `json:"schema"`
	SourceID     string                     `json:"source_id"`
	BootID       string                     `json:"boot_id"`
	WindowUptime float64                    `json:"window_uptime"`
	Workers      map[string]SourceRateUsage `json:"workers"`
}

type sourceCaptureCharge struct {
	BootID  string
	Window  float64
	Started float64
}

func sourceRateChange(dir *safefs.Directory, identity SourceIdentity, worker string, change func(*SourceRateUsage, float64, *SourceRateLedger) error) error {
	lock, err := dir.Lock("policy.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	var ledger SourceRateLedger
	if err = dir.ReadJSON("rate.json", &ledger); err != nil {
		return fmt.Errorf("source resource ledger unavailable; administrator must reconcile enrollment")
	}
	if ledger.Schema != 1 || ledger.SourceID != identity.ID {
		return fmt.Errorf("invalid source resource ledger")
	}
	boot, uptime, err := process.HostClock()
	if err != nil {
		return err
	}
	if ledger.BootID != boot || uptime-ledger.WindowUptime >= time.Hour.Seconds() {
		ledger = SourceRateLedger{1, identity.ID, boot, uptime, map[string]SourceRateUsage{}}
	}
	if uptime < ledger.WindowUptime {
		return fmt.Errorf("source monotonic clock regressed")
	}
	usage := ledger.Workers[worker]
	if err = change(&usage, uptime, &ledger); err != nil {
		return err
	}
	if ledger.Workers == nil {
		ledger.Workers = map[string]SourceRateUsage{}
	}
	ledger.Workers[worker] = usage
	return dir.WriteJSON("rate.json", ledger, true)
}

func AdmitSourceRequest(dir *safefs.Directory, identity SourceIdentity, worker string) error {
	return sourceRateChange(dir, identity, worker, func(usage *SourceRateUsage, _ float64, _ *SourceRateLedger) error {
		if usage.Requests >= SourceRequestsPerHour {
			return fmt.Errorf("source request-rate budget exhausted")
		}
		usage.Requests++
		return nil
	})
}

func ReserveSourceCapture(dir *safefs.Directory, identity SourceIdentity, worker string) (sourceCaptureCharge, error) {
	var charge sourceCaptureCharge
	err := sourceRateChange(dir, identity, worker, func(usage *SourceRateUsage, uptime float64, ledger *SourceRateLedger) error {
		if usage.CaptureSeconds+sourceSnapshotCap.Seconds() > sourceCaptureSecondsPerHour {
			return fmt.Errorf("source capture resource budget exhausted")
		}
		usage.CaptureSeconds += sourceSnapshotCap.Seconds()
		charge = sourceCaptureCharge{ledger.BootID, ledger.WindowUptime, uptime}
		return nil
	})
	return charge, err
}

func finishSourceCapture(dir *safefs.Directory, identity SourceIdentity, worker string, charge sourceCaptureCharge) error {
	return sourceRateChange(dir, identity, worker, func(usage *SourceRateUsage, uptime float64, ledger *SourceRateLedger) error {
		if ledger.BootID == charge.BootID && ledger.WindowUptime == charge.Window {
			refund := sourceSnapshotCap.Seconds() - (uptime - charge.Started)
			if refund > 0 {
				usage.CaptureSeconds -= refund
				if usage.CaptureSeconds < 0 {
					usage.CaptureSeconds = 0
				}
			}
		}
		return nil
	})
}

func AdmitSourceTransfer(dir *safefs.Directory, identity SourceIdentity, worker string, size int64) error {
	return sourceRateChange(dir, identity, worker, func(usage *SourceRateUsage, _ float64, _ *SourceRateLedger) error {
		if size < 0 || size > SourceExportBytesPerHour-usage.ExportBytes {
			return fmt.Errorf("source export byte budget exhausted")
		}
		usage.ExportBytes += size
		return nil
	})
}
