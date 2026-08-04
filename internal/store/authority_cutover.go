package store

import (
	"errors"
	"fmt"
	"strings"
)

const (
	legacyRelayOpsSHA256 = "53ADB04CF0E579DAABBF2C1540D60C82E0B03662AB691246B50F772C067C5BCA"
	legacyRelayOpsBytes  = int64(22243328)
)

var ErrAuthorityCutoverOrder = errors.New("authority cutover order is invalid")

type LegacyImageProvenance struct {
	SHA256 string
	Bytes  int64
	Path   string
	PID    int
	Owner  string
}

func (provenance LegacyImageProvenance) Validate() error {
	if strings.ToUpper(strings.TrimSpace(provenance.SHA256)) != legacyRelayOpsSHA256 || provenance.Bytes != legacyRelayOpsBytes {
		return fmt.Errorf("%w: legacy image identity differs", ErrAuthorityCutoverOrder)
	}
	if strings.TrimSpace(provenance.Path) == "" || provenance.PID <= 0 || strings.TrimSpace(provenance.Owner) == "" {
		return fmt.Errorf("%w: legacy image provenance is incomplete", ErrAuthorityCutoverOrder)
	}
	return nil
}

type AuthorityCutoverPhase string

const (
	AuthorityCutoverInitial          AuthorityCutoverPhase = "initial"
	AuthorityCutoverDraining         AuthorityCutoverPhase = "draining"
	AuthorityCutoverLegacyStopped    AuthorityCutoverPhase = "legacy_stopped"
	AuthorityCutoverBackupVerified   AuthorityCutoverPhase = "backup_verified"
	AuthorityCutoverProtocol2Ready   AuthorityCutoverPhase = "protocol2_ready"
	AuthorityCutoverCandidateRunning AuthorityCutoverPhase = "candidate_running"
	AuthorityCutoverRollbackDraining AuthorityCutoverPhase = "rollback_draining"
	AuthorityCutoverOldDBRestored    AuthorityCutoverPhase = "old_db_restored"
)

type AuthorityCutoverFence struct {
	Phase                  AuthorityCutoverPhase
	LegacyRestartDisabled  bool
	LegacyProcessAbsent    bool
	LegacyListenerClosed   bool
	LegacyDBHandlesAbsent  bool
	PreMigrationBackupHash string
	ActiveDatabaseProtocol int
}

func NewAuthorityCutoverFence() AuthorityCutoverFence {
	return AuthorityCutoverFence{Phase: AuthorityCutoverInitial, ActiveDatabaseProtocol: authorityProtocolLegacy}
}

func (fence AuthorityCutoverFence) BeginDrain() (AuthorityCutoverFence, error) {
	if fence.Phase != AuthorityCutoverInitial || fence.ActiveDatabaseProtocol != authorityProtocolLegacy {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.Phase = AuthorityCutoverDraining
	return fence, nil
}

func (fence AuthorityCutoverFence) RecordLegacyStopped(restartDisabled, processAbsent, listenerClosed, dbHandlesAbsent bool) (AuthorityCutoverFence, error) {
	if fence.Phase != AuthorityCutoverDraining || !restartDisabled || !processAbsent || !listenerClosed || !dbHandlesAbsent {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.LegacyRestartDisabled = restartDisabled
	fence.LegacyProcessAbsent = processAbsent
	fence.LegacyListenerClosed = listenerClosed
	fence.LegacyDBHandlesAbsent = dbHandlesAbsent
	fence.Phase = AuthorityCutoverLegacyStopped
	return fence, nil
}

func (fence AuthorityCutoverFence) RecordBackupVerified(digest string) (AuthorityCutoverFence, error) {
	if fence.Phase != AuthorityCutoverLegacyStopped || len(strings.TrimSpace(digest)) != 64 {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.PreMigrationBackupHash = strings.ToUpper(digest)
	fence.Phase = AuthorityCutoverBackupVerified
	return fence, nil
}

func (fence AuthorityCutoverFence) RecordProtocol2Migration() (AuthorityCutoverFence, error) {
	if fence.Phase != AuthorityCutoverBackupVerified || !fence.LegacyRestartDisabled ||
		!fence.LegacyProcessAbsent || !fence.LegacyListenerClosed || !fence.LegacyDBHandlesAbsent {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.ActiveDatabaseProtocol = authorityProtocolV2
	fence.Phase = AuthorityCutoverProtocol2Ready
	return fence, nil
}

func (fence AuthorityCutoverFence) StartCandidate(protocol int) (AuthorityCutoverFence, error) {
	if fence.Phase != AuthorityCutoverProtocol2Ready || protocol != authorityProtocolV2 ||
		fence.ActiveDatabaseProtocol != authorityProtocolV2 || !fence.LegacyRestartDisabled || !fence.LegacyProcessAbsent {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.Phase = AuthorityCutoverCandidateRunning
	return fence, nil
}

func (fence AuthorityCutoverFence) StartLegacy(protocol int) (AuthorityCutoverFence, error) {
	if protocol != authorityProtocolLegacy || fence.ActiveDatabaseProtocol != authorityProtocolLegacy ||
		fence.Phase != AuthorityCutoverOldDBRestored {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.Phase = AuthorityCutoverInitial
	return fence, nil
}

func (fence AuthorityCutoverFence) BeginRollbackDrain() (AuthorityCutoverFence, error) {
	if fence.Phase != AuthorityCutoverCandidateRunning || fence.ActiveDatabaseProtocol != authorityProtocolV2 {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.Phase = AuthorityCutoverRollbackDraining
	return fence, nil
}

func (fence AuthorityCutoverFence) RecordOldDatabaseRestored(digest string, candidateStopped, handlesAbsent bool) (AuthorityCutoverFence, error) {
	if fence.Phase != AuthorityCutoverRollbackDraining || !candidateStopped || !handlesAbsent ||
		strings.ToUpper(strings.TrimSpace(digest)) != fence.PreMigrationBackupHash {
		return fence, ErrAuthorityCutoverOrder
	}
	fence.ActiveDatabaseProtocol = authorityProtocolLegacy
	fence.Phase = AuthorityCutoverOldDBRestored
	return fence, nil
}
