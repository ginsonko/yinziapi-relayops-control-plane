package mirrorexec

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

const (
	managedChannelEnabled  = 1
	managedChannelDisabled = 2
)

var (
	ErrPlanNotExecutable   = errors.New("complete mirror plan is not executable")
	ErrNeedsReconciliation = errors.New("mirror change set needs reconciliation")
	ErrVisibilityPending   = errors.New("mirror visibility readback is pending")
	ErrInjectedCrash       = errors.New("injected executor crash")
)

// Target is the minimum already-supported target surface required by the
// complete-mirror executor. It intentionally excludes whole-channel writes.
type Target interface {
	Snapshot(context.Context, domain.TargetSite) (domain.TargetSnapshot, error)
	ApplyPricingOptionControlled(context.Context, domain.TargetSite, string, string, map[string]string, func() error) (domain.TargetMutationResult, error)

	ReadGroupModelPricing(context.Context, domain.TargetSite) (domain.GroupModelPricingContract, error)
	PreviewGroupModelPricing(context.Context, domain.TargetSite, string, domain.GroupModelPricingContract) (domain.TargetGroupModelPricingPreview, error)
	ApplyGroupModelPricingControlled(context.Context, domain.TargetSite, string, domain.GroupModelPricingContract, func() error) (domain.TargetMutationResult, error)
	RollbackGroupModelPricingControlled(context.Context, domain.TargetSite, string, domain.GroupModelPricingContract, func() error) (domain.GroupModelPricingContract, error)

	ReadManagedChannelState(context.Context, domain.TargetSite, int64) (domain.TargetManagedChannelState, error)
	ApplyManagedChannelState(context.Context, domain.TargetSite, int64, string, []string, *int, string, func() error) (domain.TargetManagedChannelMutation, error)
}

// VisibilityVerifier provides best-effort diagnostics for NewAPI's derived
// marketplace, ability and cache surfaces. Authoritative channel, pricing and
// group-ratio readbacks remain the success contract.
type VisibilityVerifier interface {
	VerifyMirrorVisibility(context.Context, domain.TargetSite, int64, string, []string) (VisibilityEvidence, error)
}

type VisibilityEvidence struct {
	MarketplaceVisible bool            `json:"marketplace_visible"`
	AbilityVisible     bool            `json:"ability_visible"`
	CacheVisible       bool            `json:"cache_visible"`
	EvidenceHash       string          `json:"evidence_hash"`
	Details            json.RawMessage `json:"details,omitempty"`
}

type CrashHook func(boundary string) error

type Options struct {
	WorkerID    string
	LeaseTTL    time.Duration
	MaxAttempts int
	Now         func() time.Time
	CrashHook   CrashHook
}

type ApplyInput struct {
	Policy          mirror.Policy
	Plan            mirror.CompleteMirrorPlan
	Target          domain.TargetSite
	ChannelID       int64
	SourceProof     json.RawMessage
	SourceIdentity  string
	OperationKey    string
	ApprovalGranted bool
}

type Receipt struct {
	ChangeSet store.MirrorChangeSet `json:"change_set"`
	Replayed  bool                  `json:"replayed"`
}

type Executor struct {
	store      *store.Store
	target     Target
	visibility VisibilityVerifier
	options    Options
}

func New(st *store.Store, target Target, visibility VisibilityVerifier, options Options) (*Executor, error) {
	if st == nil || target == nil {
		return nil, errors.New("mirror executor store and target are required")
	}
	if options.WorkerID == "" {
		options.WorkerID = "mirror-executor"
	}
	if options.LeaseTTL <= 0 {
		options.LeaseTTL = 10 * time.Minute
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 2
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Executor{store: st, target: target, visibility: visibility, options: options}, nil
}

type channelState struct {
	ChannelID int64    `json:"channel_id"`
	Models    []string `json:"models"`
	Status    int      `json:"status"`
	CASHash   string   `json:"cas_hash,omitempty"`
}

type ratioState struct {
	Option string            `json:"option"`
	Values map[string]string `json:"values"`
	Hash   string            `json:"hash"`
}

type readbackState struct {
	ChannelSemanticHash  string `json:"channel_semantic_hash"`
	ContractRevision     string `json:"contract_revision"`
	GroupRatioHash       string `json:"group_ratio_hash"`
	TargetFunctionalHash string `json:"target_functional_hash"`
	VisibilityVerified   bool   `json:"visibility_verified"`
}

type stepIntent struct {
	RequestID       string          `json:"request_id"`
	ResourceKind    string          `json:"resource_kind"`
	ResourceID      string          `json:"resource_id"`
	CASExpectedHash string          `json:"cas_expected_hash"`
	Before          json.RawMessage `json:"before"`
	Desired         json.RawMessage `json:"desired"`
}

type stepResult struct {
	ResultState  string          `json:"result_state"`
	ReadbackHash string          `json:"readback_hash,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type finalReadback struct {
	Channel        domain.TargetManagedChannelState `json:"channel"`
	Contract       domain.GroupModelPricingContract `json:"contract"`
	GroupRatio     domain.TargetPriceOption         `json:"group_ratio"`
	FunctionalHash string                           `json:"functional_hash"`
	Visibility     VisibilityEvidence               `json:"visibility"`
}

type materializedChange struct {
	ChangeSet        store.MirrorChangeSet
	ExpectedOwned    store.MirrorOwnershipState
	DesiredOwnedHint json.RawMessage
}
