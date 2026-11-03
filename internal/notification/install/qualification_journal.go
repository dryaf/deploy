package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxQualificationRecords = 64

type QualificationRecord struct {
	Version    int                 `json:"version"`
	Sequence   int                 `json:"sequence"`
	Previous   string              `json:"previous,omitempty"`
	Report     QualificationReport `json:"report"`
	RecordedAt time.Time           `json:"recorded_at"`
}

type qualificationHead struct {
	Version      int    `json:"version"`
	Installation string `json:"installation"`
	Sequence     int    `json:"sequence"`
	Hash         string `json:"hash"`
}

type qualificationHistory struct {
	Head    qualificationHead
	Records []QualificationRecord
	Pending bool
}

func qualificationRecordName(sequence int) string { return fmt.Sprintf("%04d.json", sequence) }

// The independent head detects a missing committed tail. A record written before
// the head is pending evidence; only an exact retry can acknowledge it. Neither
// reading status nor starting a worker completes an operator's interrupted call.
func readQualificationHistory(d *safefs.Directory, installation string) (h qualificationHistory, err error) {
	h.Head = qualificationHead{Version: 1, Installation: installation}
	headErr := d.ReadJSON("qualification-head.json", &h.Head)
	if headErr != nil && !os.IsNotExist(headErr) {
		return h, headErr
	}
	if headErr == nil && (h.Head.Version != 1 || h.Head.Installation != installation || h.Head.Sequence < 1 || h.Head.Sequence > maxQualificationRecords || !safefs.DigestPattern.MatchString(h.Head.Hash)) {
		return h, fmt.Errorf("invalid qualification head")
	}
	q, err := d.Sub("qualifications")
	if os.IsNotExist(err) && os.IsNotExist(headErr) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	defer q.Close()
	names, err := q.File().Readdirnames(maxQualificationRecords + 33)
	if err != nil && !errors.Is(err, io.EOF) {
		return h, err
	}
	temporaries := 0
	names = slices.DeleteFunc(names, func(name string) bool {
		if safefs.IsJSONTemporary(name) {
			temporaries++
			return true
		}
		return false
	})
	if len(names) > maxQualificationRecords || temporaries > 32 {
		return h, fmt.Errorf("qualification history exceeds budget")
	}
	slices.Sort(names)
	previous := ""
	var lastTime time.Time
	ids := map[string]bool{}
	for i, name := range names {
		if name != qualificationRecordName(i+1) {
			return h, fmt.Errorf("qualification history missing or unknown record")
		}
		var record QualificationRecord
		raw, e := ownedManagedFile(q, name, 1<<20)
		if e != nil {
			return h, e
		}
		if e = safefs.DecodeJSON(raw, &record, 1<<20); e != nil {
			return h, e
		}
		if record.Version != 1 || record.Sequence != i+1 || record.Previous != previous || record.Report.validate() != nil || record.Report.Binding.Manifest.Installation != installation || ids[record.Report.ID] || record.RecordedAt.Before(record.Report.CreatedAt) || !record.RecordedAt.Before(record.Report.CreatedAt.Add(qualificationWindow)) || record.RecordedAt.Before(lastTime) {
			return h, fmt.Errorf("qualification history identity, time or hash changed")
		}
		for _, check := range record.Report.Checks {
			if check.ObservedAt.After(record.RecordedAt) {
				return h, fmt.Errorf("qualification observation follows its recorded result")
			}
		}
		ids[record.Report.ID] = true
		h.Records = append(h.Records, record)
		previous, lastTime = protocol.Hash(record), record.RecordedAt
		if record.Sequence == h.Head.Sequence && previous != h.Head.Hash {
			return h, fmt.Errorf("qualification committed head changed")
		}
	}
	if len(h.Records) < h.Head.Sequence || len(h.Records) > h.Head.Sequence+1 {
		return h, fmt.Errorf("qualification record and head diverged")
	}
	h.Pending = len(h.Records) > h.Head.Sequence
	return h, nil
}

func appendQualification(d *safefs.Directory, report QualificationReport, now time.Time, fault func(string) error) (record QualificationRecord, err error) {
	if err = report.validate(); err != nil {
		return
	}
	raw, err := json.Marshal(report)
	if err != nil || len(raw) > (1<<20)-4096 {
		return record, fmt.Errorf("qualification report exceeds record budget")
	}
	for _, check := range report.Checks {
		if check.ObservedAt.After(now) {
			return record, fmt.Errorf("qualification observation is in the future")
		}
	}
	h, err := readQualificationHistory(d, report.Binding.Manifest.Installation)
	if err != nil {
		return record, err
	}
	for _, old := range h.Records {
		if old.Report.ID == report.ID {
			if protocol.Hash(old.Report) != protocol.Hash(report) {
				return record, fmt.Errorf("qualification report ID reused with different evidence")
			}
			if old.Sequence <= h.Head.Sequence {
				return old, nil
			}
			record = old
		}
	}
	if h.Pending && record.Sequence == 0 {
		return record, fmt.Errorf("qualification acknowledgement pending; retry the exact report first")
	}
	if len(h.Records) > 0 && now.Before(h.Records[len(h.Records)-1].RecordedAt) {
		return record, fmt.Errorf("qualification clock moved backward")
	}
	if record.Sequence == 0 {
		if len(h.Records) >= maxQualificationRecords {
			return record, fmt.Errorf("qualification history is full; evidence is not evicted")
		}
		if now.Before(report.CreatedAt) || !now.Before(report.CreatedAt.Add(qualificationWindow)) || (len(h.Records) > 0 && now.Before(h.Records[len(h.Records)-1].RecordedAt)) {
			return record, fmt.Errorf("qualification clock moved backward or report expired")
		}
		if err = d.Capacity(2 << 20); err != nil {
			return record, err
		}
		record = QualificationRecord{1, h.Head.Sequence + 1, h.Head.Hash, report, now.UTC()}
		if err = d.Mkdir("qualifications"); err != nil {
			return record, err
		}
		if err = d.WriteJSON("qualifications/"+qualificationRecordName(record.Sequence), record, false); err != nil {
			return record, err
		}
		if fault != nil {
			if err = fault("qualification_record"); err != nil {
				return record, err
			}
		}
	}
	head := qualificationHead{1, report.Binding.Manifest.Installation, record.Sequence, protocol.Hash(record)}
	if err = d.WriteJSON("qualification-head.json", head, h.Head.Sequence != 0); err != nil {
		return record, err
	}
	if fault != nil {
		err = fault("qualification_head")
	}
	return
}
