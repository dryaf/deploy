package worker

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const snapshotRecoveryFile = "snapshot-recovery.json"

// This evidence makes a recovered snapshot inspectable without erasing the
// quarantine marker. It grants no authority; a later recovery must independently
// retain and verify the ancestor publication and current ownership histories.
type snapshotRecoveryEvidence struct {
	RuntimeFiles []recoveryEvidenceFile    `json:"runtime_files,omitempty"`
	Version      int                       `json:"version"`
	Publication  RecoveryPublicationCommit `json:"publication"`
	Proof        RecoveryCheckpointProof   `json:"proof"`
	Adoption     RecoveryVerification      `json:"adoption"`
}

func validSnapshotVersion(m SnapshotManifest) bool {
	return m.Version == 1 && m.RecoveryHash == "" || m.Version == 2 && safefs.DigestPattern.MatchString(m.RecoveryHash)
}

func (e snapshotRecoveryEvidence) validate(m SnapshotManifest, c Config) error {
	if err := validateSnapshotRuntimeInventory(e.RuntimeFiles, c); err != nil {
		return err
	}
	p, proof := e.Publication, e.Proof
	if e.Version != 1 || m.Version != 2 || protocol.Hash(e) != m.RecoveryHash || p.Version != 1 || proof.Version != 1 || proof.After != m.Identity || proof.ConfigHash != protocol.Hash(c) || p.ConfigHash != proof.ConfigHash || proof.PendingStages < 0 || proof.PublicationAllowed || proof.DeliveryEnabled || proof.ExecutionEnabled {
		return fmt.Errorf("invalid snapshot recovery evidence")
	}
	ph := proof.ProofHash
	proof.ProofHash = ""
	if !safefs.DigestPattern.MatchString(ph) || protocol.Hash(proof) != ph {
		return fmt.Errorf("snapshot recovery proof digest changed")
	}
	ch := p.CommitHash
	p.CommitHash = ""
	if !safefs.DigestPattern.MatchString(ch) || protocol.Hash(p) != ch {
		return fmt.Errorf("snapshot publication digest changed")
	}
	image := p.Image
	ih := image.ImageHash
	image.ImageHash = ""
	if !safefs.DigestPattern.MatchString(ih) || protocol.Hash(image) != ih || p.Image.Version != 1 || p.Image.ProofHash != ph || p.Image.Operation != proof.Operation || p.Image.ReviewHash != proof.ReviewHash || p.Image.CheckpointHash != proof.CheckpointHash || p.Image.AnchorHash != protocol.Hash(recoveryPublicationAnchor{1, "fenced", e.Proof}) || p.OriginalSourceHash != proof.OriginalSourceHash || p.OriginalDatabaseHash != proof.OriginalDatabaseHash || p.Image.PublicationAllowed || p.Image.DeliveryEnabled || p.Image.ExecutionEnabled {
		return fmt.Errorf("snapshot publication does not bind its recovery proof")
	}
	want := RecoveryVerification{1, ch, protocol.Hash(c), proof.Before, proof.After, true}
	if e.Adoption != want {
		return fmt.Errorf("snapshot adoption changed")
	}
	return nil
}

func readSnapshotRecovery(d *safefs.Directory, m SnapshotManifest, c Config) (*snapshotRecoveryEvidence, error) {
	var evidence snapshotRecoveryEvidence
	err := readCheckpointJSON(d, snapshotRecoveryFile, &evidence)
	if m.Version == 1 {
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("ordinary snapshot has unexpected recovery evidence")
	}
	if err != nil {
		return nil, err
	}
	if err = evidence.validate(m, c); err != nil {
		return nil, err
	}
	return &evidence, nil
}

func (s *Store) snapshotRecoveryEvidence(ctx context.Context) (*snapshotRecoveryEvidence, error) {
	if s.recoveryCommitHash == "" {
		return nil, nil
	}
	if err := s.checkRetirementLease(); err != nil {
		return nil, err
	}
	publication, proof, err := verifyCompletedPublication(ctx, s.dir, s.lease, s.config, s.recoveryCommitHash)
	if err != nil {
		return nil, err
	}
	if publication != s.recoveryPublication || proof != s.recoveryProof || proof.After != s.identity {
		return nil, fmt.Errorf("snapshot recovery authority changed")
	}
	adoption := RecoveryVerification{1, publication.CommitHash, protocol.Hash(s.config), proof.Before, proof.After, true}
	for _, name := range []string{recoveryVerificationIntent, recoveryVerificationComplete} {
		ok, err := verificationRecord(s.dir, name, adoption)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("snapshot requires completed recovery adoption")
		}
	}
	head, err := s.commandSQLHead()
	if err != nil {
		return nil, err
	}
	hash := ""
	if head != nil {
		hash = head.Hash
	}
	if err = s.RequireManagedCommandHistory(hash); err != nil {
		return nil, err
	}
	files, err := snapshotRuntimeInventory(ctx, s.dir, s.config)
	if err != nil {
		return nil, err
	}
	return &snapshotRecoveryEvidence{Version: 1, Publication: publication, Proof: proof, Adoption: adoption, RuntimeFiles: files}, nil
}
