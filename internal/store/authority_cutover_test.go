package store

import (
	"errors"
	"testing"
)

func TestR17LegacyProvenance(t *testing.T) {
	valid := LegacyImageProvenance{SHA256: legacyRelayOpsSHA256, Bytes: legacyRelayOpsBytes,
		Path: `C:\release\relayops.exe`, PID: 42, Owner: "service:relayops"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Bytes++
	if err := invalid.Validate(); !errors.Is(err, ErrAuthorityCutoverOrder) {
		t.Fatalf("wrong legacy image passed: %v", err)
	}
}

func TestR17LegacyDrainNoHandle(t *testing.T) {
	fence, err := NewAuthorityCutoverFence().BeginDrain()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fence.RecordLegacyStopped(true, true, true, false); !errors.Is(err, ErrAuthorityCutoverOrder) {
		t.Fatalf("database handle was ignored: %v", err)
	}
	fence, err = fence.RecordLegacyStopped(true, true, true, true)
	if err != nil || fence.Phase != AuthorityCutoverLegacyStopped {
		t.Fatalf("valid drain failed: fence=%+v err=%v", fence, err)
	}
}

func TestR17LegacyNeverOpensMigrated(t *testing.T) {
	fence := readyProtocol2Fence(t)
	if _, err := fence.StartLegacy(authorityProtocolLegacy); !errors.Is(err, ErrAuthorityCutoverOrder) {
		t.Fatalf("legacy writer opened protocol-2 database: %v", err)
	}
	if _, err := fence.StartCandidate(authorityProtocolV2); err != nil {
		t.Fatal(err)
	}
}

func TestR17RollbackOldDBOnly(t *testing.T) {
	fence := readyProtocol2Fence(t)
	var err error
	fence, err = fence.StartCandidate(authorityProtocolV2)
	if err != nil {
		t.Fatal(err)
	}
	fence, err = fence.BeginRollbackDrain()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fence.StartLegacy(authorityProtocolLegacy); !errors.Is(err, ErrAuthorityCutoverOrder) {
		t.Fatalf("legacy writer started before restore: %v", err)
	}
	if _, err := fence.RecordOldDatabaseRestored("00", true, true); !errors.Is(err, ErrAuthorityCutoverOrder) {
		t.Fatalf("wrong backup digest restored: %v", err)
	}
	fence, err = fence.RecordOldDatabaseRestored(testBackupDigest, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fence.StartLegacy(authorityProtocolLegacy); err != nil {
		t.Fatal(err)
	}
}

const testBackupDigest = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"

func readyProtocol2Fence(t *testing.T) AuthorityCutoverFence {
	t.Helper()
	fence, err := NewAuthorityCutoverFence().BeginDrain()
	if err != nil {
		t.Fatal(err)
	}
	fence, err = fence.RecordLegacyStopped(true, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	fence, err = fence.RecordBackupVerified(testBackupDigest)
	if err != nil {
		t.Fatal(err)
	}
	fence, err = fence.RecordProtocol2Migration()
	if err != nil {
		t.Fatal(err)
	}
	return fence
}
