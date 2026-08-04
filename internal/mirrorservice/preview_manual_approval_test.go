package mirrorservice

import (
	"context"
	"errors"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorexec"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

type manualApprovalVisibility struct{}

func (manualApprovalVisibility) VerifyMirrorVisibility(context.Context, domain.TargetSite, int64,
	string, []string) (mirrorexec.VisibilityEvidence, error) {
	return mirrorexec.VisibilityEvidence{}, errors.New("unexpected visibility verification")
}

func TestUnconfirmedEvidenceDoesNotBlockExplicitAdministratorApplyPreview(t *testing.T) {
	service := &Service{remote: remotewrite.New(true), visibility: manualApprovalVisibility{}}
	service.SetAutomaticWritesEnabled(true)
	policy := mirror.Policy{Enabled: true}
	target := domain.TargetSite{Enabled: true, WriteMode: "auto"}
	plan := mirror.CompleteMirrorPlan{Decision: mirror.DecisionAutomatic}

	gates := service.serviceGates(policy, target, plan, false)
	if hasBlockingServiceGate(gates) {
		t.Fatalf("unconfirmed evidence unexpectedly blocks an administrator preview: %#v", gates)
	}
	if len(gates) != 1 || gates[0].Code != "source_evidence_unconfirmed" || gates[0].BlocksApply {
		t.Fatalf("evidence gate = %#v", gates)
	}
}

func TestUnconfirmedEvidenceStillRequiresExplicitAdministratorApproval(t *testing.T) {
	preview := Preview{EvidenceReady: false, Executable: true}
	if err := requireEvidenceApproval(preview, false); !errors.Is(err, ErrEvidenceUnconfirmed) {
		t.Fatalf("approval error = %v", err)
	}
	if err := requireEvidenceApproval(preview, true); err != nil {
		t.Fatalf("explicit administrator approval was rejected: %v", err)
	}
	preview.EvidenceReady = true
	if err := requireEvidenceApproval(preview, false); err != nil {
		t.Fatalf("confirmed evidence unexpectedly requires approval: %v", err)
	}
}
