package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// ProducerReservation is a specialist-owned identity. Reserving it neither
// enrolls source keys nor grants a gateway generation production authority.
// Archive writer epochs remain a separate, archive-owned counter.
type ProducerReservation struct {
	Version    int             `json:"version"`
	Producer   notify.Producer `json:"producer"`
	Generation string          `json:"generation"`
}

func (r ProducerReservation) Validate(workerID string) error {
	if r.Version != 1 || r.Producer.ID != workerID || !safefs.ValidID(workerID, "w") || !safefs.ValidID(r.Producer.Epoch, "n") || !safefs.ValidID(r.Generation, "g") {
		return fmt.Errorf("invalid backup producer reservation")
	}
	return nil
}

// ReserveWorker uses the same pinned request and setup journal as SetupWorker.
// The target returns immediately after durable identity reservation. No source
// enrollment, archive initialization, image pull or service installation occurs.
func ReserveWorker(ctx context.Context, plan SetupPlan) (WorkerSetupResponse, error) {
	if plan.Request.Declaration == nil || plan.Request.Recover {
		return WorkerSetupResponse{}, fmt.Errorf("producer reservation requires a fresh declarative installation")
	}
	plan.Request.Operation = "reserve"
	return SetupWorker(ctx, plan)
}

func reserveSetupProducer(d *safefs.Directory, j *WorkerSetupJournal) (ProducerReservation, error) {
	if j.Declaration == nil || j.Updating || j.RecoveryNeeded {
		return ProducerReservation{}, fmt.Errorf("backup lifecycle is not available for producer reservation")
	}
	if j.Shared != nil && (j.Producer == nil || j.Shared.Owner.Binding.Producer != j.Producer.Producer || j.Shared.Owner.Binding.Generation != j.Producer.Generation || j.Shared.GatewayRoot != j.Declaration.NotificationRoot || !reportRouteOwnerShape(*j, j.Shared.Owner)) {
		return ProducerReservation{}, fmt.Errorf("shared reporting ownership changed from the declarative reservation; migration required")
	}
	var intent producerReservationIntent
	err := d.ReadJSON("producer-reservation.json", &intent)
	if err == nil {
		if intent.Version != 1 || intent.SetupID != j.ID || intent.WorkerID != j.WorkerID || intent.ArchiveID != j.ArchiveID || intent.Reservation.Validate(j.WorkerID) != nil || j.Producer != nil && *j.Producer != intent.Reservation {
			return ProducerReservation{}, fmt.Errorf("backup producer reservation authority changed")
		}
		if j.Producer == nil {
			if intent.RequestHash != j.RequestHash || j.Stage != "started" {
				return ProducerReservation{}, fmt.Errorf("backup producer reservation completion requires its original setup")
			}
			next := *j
			next.Producer = &intent.Reservation
			if err = d.WriteJSON("setup.json", next, true); err != nil {
				return ProducerReservation{}, err
			}
			*j = next
		}
		return intent.Reservation, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return ProducerReservation{}, err
	}
	if j.Producer != nil {
		return ProducerReservation{}, fmt.Errorf("backup producer reservation record missing; recovery required")
	}
	// Once installed, absence is not proof that an identity was never published.
	// Existing installations need an explicit adoption decision.
	if j.Stage != "started" || j.StateInitialized || j.StateInitializing || len(j.SourceEnrollments) != 0 || len(j.SourceKeys) != 0 {
		return ProducerReservation{}, fmt.Errorf("existing backup requires producer adoption; reservation is unavailable")
	}
	epoch, err := safefs.NewID("n")
	if err != nil {
		return ProducerReservation{}, err
	}
	generation, err := safefs.NewID("g")
	if err != nil {
		return ProducerReservation{}, err
	}
	r := ProducerReservation{Version: 1, Producer: notify.Producer{ID: j.WorkerID, Epoch: epoch}, Generation: generation}
	intent = producerReservationIntent{Version: 1, SetupID: j.ID, RequestHash: j.RequestHash, WorkerID: j.WorkerID, ArchiveID: j.ArchiveID, Reservation: r}
	if err = d.WriteJSON("producer-reservation.json", intent, false); err != nil {
		return ProducerReservation{}, err
	}
	next := *j
	next.Producer = &r
	if err = d.WriteJSON("setup.json", next, true); err != nil {
		return ProducerReservation{}, err
	}
	*j = next
	return r, nil
}

type producerReservationIntent struct {
	Version     int                 `json:"version"`
	SetupID     string              `json:"setup_id"`
	RequestHash string              `json:"request_hash"`
	WorkerID    string              `json:"worker_id"`
	ArchiveID   string              `json:"archive_id"`
	Reservation ProducerReservation `json:"reservation"`
}

// An ordinary specialist-only edit may reuse an already reserved producer. This
// is read-only: the old setup remains authoritative until its existing quiesce/
// update transaction commits. A changed gateway scope cannot take this path.
func inspectSetupUpdateReservation(root, stateRoot *safefs.Directory, j *WorkerSetupJournal, request WorkerSetupRequest) (ProducerReservation, error) {
	var zero ProducerReservation
	if err := requireSharedHelperRequest(*j, request); err != nil {
		return zero, err
	}
	if j.Stage != "complete" || j.Updating || j.RecoveryNeeded || j.Declaration == nil || request.Declaration == nil || j.Producer == nil || j.Declaration.NotificationDesiredHash == "" || j.Declaration.NotificationDesiredHash != request.Declaration.NotificationDesiredHash {
		return zero, fmt.Errorf("producer reuse requires a completed setup and unchanged notification policy scope")
	}
	if err := checkSetupProducerReservation(stateRoot, j); err != nil {
		return zero, err
	}
	var before WorkerInstallation
	if err := readInstalledWorkerJSON(root, &before); err != nil {
		return zero, err
	}
	if before.WorkerID != j.WorkerID || before.ArchiveID != j.ArchiveID || validateWorkerInstallation(before) != nil {
		return zero, fmt.Errorf("reserved backup installation changed")
	}
	if err := validateDeclarativeBackupUpdate(before, j.Declaration, request); err != nil {
		return zero, err
	}
	added, removed := setupSourceChanges(before, request)
	helperChanged := j.Declaration.HelperSHA256 != request.Declaration.HelperSHA256
	if before.RelayImage != request.RelayImage || len(added) == 0 && len(removed) == 0 && !reflect.DeepEqual(j.Declaration.Sources, request.Declaration.Sources) {
		return zero, fmt.Errorf("shared source-helper or relay upgrades require their own reviewed transition")
	}
	if before.Image != request.Image || helperChanged {
		view := *j
		review := request
		review.Operation = "apply"
		if err := prepareSharedPolicyHistory(root, &view, review); err != nil {
			return zero, err
		}
		version := 6
		if helperChanged {
			version = 7
			if view.SharedPolicyUpdate != nil && view.SharedPolicyUpdate.Version == 8 {
				version = 8
			}
		}
		if view.SharedPolicyUpdate == nil || view.SharedPolicyUpdate.Version != version {
			return zero, fmt.Errorf("shared artifact update requires retained reporting ownership")
		}
	}
	if len(added) > 0 || len(removed) > 0 {
		// Review both sides in memory before returning a reusable producer. The
		// existing update transaction remains the only publisher of source authority.
		view := *j
		if err := prepareSetupSourceRetirement(root, &view, before, request); err != nil {
			return zero, err
		}
		if err := prepareSetupSourceExpansion(root, &view, before, request); err != nil {
			return zero, err
		}
	} else if err := verifySetupSourceKeys(root, *j); err != nil {
		return zero, err
	}

	for path, hash := range j.OwnedFiles {
		body, err := readBackupOwnedFile(root, path, 8<<20)
		if err != nil || sharedSetupDigest(body) != hash {
			return zero, fmt.Errorf("reserved backup owned file changed")
		}
	}
	return *j.Producer, nil
}

// A disconnect can leave an update prepared before installation recovery is
// armed. Reuse only its exact retained producer and unchanged gateway scope. This
// result authorizes resuming backup setup, never gateway enrollment/admission.
func inspectPendingSetupUpdateReservation(root, stateRoot *safefs.Directory, j *WorkerSetupJournal, request WorkerSetupRequest) (ProducerReservation, error) {
	var zero ProducerReservation
	previous, next := j.PreviousDeclaration, j.Declaration
	if !j.Updating || j.RecoveryNeeded || j.Stage != "prepared" || previous == nil || next == nil || request.Declaration == nil || j.Producer == nil || workerSetupRequestHash(request) != j.RequestHash || j.PreviousRequestHash == "" || !reflect.DeepEqual(next, request.Declaration) {
		return zero, fmt.Errorf("pending backup update requires its exact request and predecessor")
	}
	helperUpgrade := j.SharedPolicyUpdate != nil && sharedHelperVersion(j.SharedPolicyUpdate.Version) && checkSharedPolicyUpdate(*j) == nil
	if next.NotificationDesiredHash == "" || previous.NotificationDesiredHash != next.NotificationDesiredHash || previous.NotificationRoot != next.NotificationRoot || previous.NotificationRoute != next.NotificationRoute || (previous.HelperSHA256 != next.HelperSHA256 && !helperUpgrade) || j.SourceExpansion == nil && j.SourceRetirement == nil && !reflect.DeepEqual(previous.Sources, next.Sources) {
		return zero, fmt.Errorf("pending backup update changed gateway or artifact authority")
	}
	if err := validateSetupDesired(previous); err != nil {
		return zero, err
	}
	if err := validateSetupDesired(next); err != nil {
		return zero, err
	}
	if err := checkSetupProducerReservation(stateRoot, j); err != nil {
		return zero, err
	}
	var before WorkerInstallation
	if err := readInstalledWorkerJSON(root, &before); err != nil {
		return zero, err
	}
	if before.WorkerID != j.WorkerID || before.ArchiveID != j.ArchiveID || validateWorkerInstallation(before) != nil || before.RelayImage != request.RelayImage {
		return zero, fmt.Errorf("pending backup update lost its original runtime")
	}
	if before.Image != request.Image {
		if j.SharedPolicyUpdate == nil || (j.SharedPolicyUpdate.Version != 6 && !sharedHelperVersion(j.SharedPolicyUpdate.Version)) || checkSharedPolicyUpdate(*j) != nil {
			return zero, fmt.Errorf("pending image update lost its retained review")
		}
	}
	if helperUpgrade {
		review, err := reviewSharedHelper(root, j.SharedPolicyUpdate.Before, request.Declaration.HelperSHA256)
		if err != nil {
			return zero, err
		}
		if !reflect.DeepEqual(review, j.SharedPolicyUpdate.Helper) {
			return zero, fmt.Errorf("pending upgrade lost its old helper")
		}
	}
	if err := validateDeclarativeBackupUpdate(before, previous, request); err != nil {
		return zero, err
	}
	if j.SourceExpansion != nil {
		if err := checkSetupSourceExpansion(root, *j, &request); err != nil {
			return zero, err
		}
	} else {
		if err := verifySetupSourceKeys(root, *j); err != nil {
			return zero, err
		}
	}
	for path, hash := range j.OwnedFiles {
		body, err := readBackupOwnedFile(root, path, 8<<20)
		if err != nil || sharedSetupDigest(body) != hash {
			return zero, fmt.Errorf("pending backup update has changed owned files")
		}
	}
	return *j.Producer, nil
}

// Every subsequent setup checks the independent reservation intent, including
// ordinary updates. Losing either record cannot authorize a fresh producer.
func checkSetupProducerReservation(d *safefs.Directory, j *WorkerSetupJournal) error {
	var intent producerReservationIntent
	err := d.ReadJSON("producer-reservation.json", &intent)
	if errors.Is(err, os.ErrNotExist) && j.Producer == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup producer reservation unavailable; recovery required")
	}
	if intent.Version != 1 || intent.SetupID != j.ID || intent.WorkerID != j.WorkerID || intent.ArchiveID != j.ArchiveID || intent.Reservation.Validate(j.WorkerID) != nil {
		return fmt.Errorf("backup producer reservation authority changed")
	}
	if j.Producer == nil {
		_, err = reserveSetupProducer(d, j)
		return err
	}
	if *j.Producer != intent.Reservation {
		return fmt.Errorf("backup producer reservation differs from setup journal")
	}
	return nil
}

// InspectReservedProducer is read-only. Absence means no backup identity has
// been reserved yet; partial or lost authority is an error, never a new epoch.
func InspectReservedProducer(home, desiredHash, gatewayRoot, route string) (ProducerReservation, bool, error) {
	var zero ProducerReservation
	root, err := safefs.Open(home)
	if err != nil {
		return zero, false, err
	}
	defer root.Close()
	path := ".local/state/deploy/backup-worker/setup.json"
	var j WorkerSetupJournal
	err = root.ReadJSON(path, &j)
	if errors.Is(err, os.ErrNotExist) {
		for _, candidate := range []string{".local/state/deploy/locks/backup-worker-setup.lock", ".local/state/deploy/backup-worker/producer-reservation.json", ".local/state/deploy/backup-worker/state"} {
			f, e := root.Open(candidate, os.O_RDONLY, 0)
			if e == nil {
				f.Close()
				return zero, false, fmt.Errorf("backup reservation journal missing; recovery required")
			}
			if !errors.Is(e, os.ErrNotExist) {
				return zero, false, e
			}
		}
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return zero, false, err
	}
	defer lock.Close()
	if err = root.ReadJSON(path, &j); err != nil {
		return zero, false, err
	}
	expected := ""
	if j.Declaration != nil {
		expected = j.Declaration.DesiredHash
		if j.Declaration.NotificationDesiredHash != "" {
			expected = j.Declaration.NotificationDesiredHash
		}
	}
	if j.Declaration == nil || expected != desiredHash || j.Declaration.NotificationRoot != gatewayRoot || j.Declaration.NotificationRoute != route || j.Declaration.Home != home || j.Declaration.UID != os.Getuid() || j.Producer == nil || j.Producer.Validate(j.WorkerID) != nil || j.RecoveryNeeded || j.Updating || j.SourceExpansion != nil || j.SourceRetirement != nil || j.NotificationScope != nil || j.Stage == "uninstalled" {
		return zero, false, fmt.Errorf("backup producer requires the matching declarative reservation or recovery")
	}
	d, err := root.Sub(filepath.Dir(path))
	if err != nil {
		return zero, false, err
	}
	defer d.Close()
	if err = checkSetupProducerReservation(d, &j); err != nil {
		return zero, false, err
	}
	if err = verifyNotificationScopeHistory(root, j); err != nil {
		return zero, false, err
	}
	return *j.Producer, true, nil
}
