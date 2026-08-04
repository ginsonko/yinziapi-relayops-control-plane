package mirrorservice

import (
	"encoding/json"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

const (
	defaultProofLimit  = 12
	defaultPreviewTTL  = 15 * time.Minute
	defaultPreviewSize = 512
)

// Options bounds evidence and ephemeral approval previews. Preview state is
// intentionally process-local: a restart requires a fresh read of both source
// and target state instead of reviving a stale approval.
type Options struct {
	EvidenceMaximumAge   time.Duration
	PreviewTTL           time.Duration
	PreviewCapacity      int
	ProofLimit           int
	ConfirmationAttempts int
	ConfirmationInterval time.Duration
	Now                  func() time.Time
}

type PreviewRequest struct {
	PolicyUUID string `json:"policy_uuid"`
}

type ApplyRequest struct {
	PreviewID       string `json:"preview_id"`
	ApprovalGranted bool   `json:"approval_granted"`
}

type Confirmation struct {
	Kind   mirror.ChangeKind         `json:"kind"`
	Result mirror.ConfirmationResult `json:"result"`
}

type ServiceGate struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	BlocksApply bool   `json:"blocks_apply"`
}

// Preview is the redacted common contract rendered by every product entry.
// It excludes raw collection proof JSON, credentials, target secrets and
// executor intent payloads.
type Preview struct {
	ID                   string                    `json:"id"`
	Policy               mirror.Policy             `json:"policy"`
	ChannelID            int64                     `json:"channel_id"`
	SourceState          mirror.SourceGroupState   `json:"source_state"`
	SourceRevision       string                    `json:"source_revision"`
	SourceSemanticHash   string                    `json:"source_semantic_hash"`
	SourceObservedAt     time.Time                 `json:"source_observed_at"`
	SnapshotResponseHash string                    `json:"snapshot_response_hash"`
	Confirmations        []Confirmation            `json:"confirmations"`
	EvidenceReady        bool                      `json:"evidence_ready"`
	Plan                 mirror.CompleteMirrorPlan `json:"plan"`
	ServiceGates         []ServiceGate             `json:"service_gates,omitempty"`
	Executable           bool                      `json:"executable"`
	GeneratedAt          time.Time                 `json:"generated_at"`
}

type ApplyReceipt struct {
	PreviewID string                `json:"preview_id"`
	Receipt   store.MirrorChangeSet `json:"change_set"`
	Replayed  bool                  `json:"replayed"`
}

type cachedPreview struct {
	public         Preview
	sourceProof    json.RawMessage
	sourceIdentity string
	expiresAt      time.Time
}

type sourceEvidence struct {
	SupplierID           int64                    `json:"supplier_id"`
	SourceGroup          string                   `json:"source_group"`
	SourceState          mirror.SourceGroupState  `json:"source_state"`
	SourceSemanticHash   string                   `json:"source_semantic_hash"`
	SnapshotResponseHash string                   `json:"snapshot_response_hash"`
	Proofs               []mirror.CollectionProof `json:"proofs"`
}

type AdoptionPreviewRequest struct {
	ChannelID int64                 `json:"channel_id"`
	Policy    mirror.PolicyDocument `json:"policy"`
}

type AdoptionApplyRequest struct {
	PreviewID       string `json:"preview_id"`
	ApprovalGranted bool   `json:"approval_granted"`
}

// AdoptionPreview independently claims the current channel membership and the
// selected group's pricing contract. Their model sets may differ or be empty;
// the first mirror run converges both to the authoritative upstream set.
type AdoptionPreview struct {
	ID                      string                                     `json:"id"`
	Policy                  mirror.Policy                              `json:"policy"`
	Channel                 domain.TargetChannel                       `json:"channel"`
	ManagedState            domain.TargetManagedChannelState           `json:"managed_state"`
	ContractRevision        string                                     `json:"contract_revision"`
	DesiredContractRevision string                                     `json:"desired_contract_revision"`
	ContractModels          map[string]domain.GroupModelPricingSetting `json:"contract_models"`
	SourceModelCount        int                                        `json:"source_model_count"`
	SourceTargetOverlap     int                                        `json:"source_target_overlap"`
	ServiceGates            []ServiceGate                              `json:"service_gates,omitempty"`
	AlreadyAdopted          bool                                       `json:"already_adopted"`
	Executable              bool                                       `json:"executable"`
	GeneratedAt             time.Time                                  `json:"generated_at"`
}

type AdoptionReceipt struct {
	PreviewID        string        `json:"preview_id"`
	Policy           mirror.Policy `json:"policy"`
	ChannelID        int64         `json:"channel_id"`
	ContractRevision string        `json:"contract_revision"`
	RemoteStamped    bool          `json:"remote_stamped"`
	Replayed         bool          `json:"replayed"`
}

type cachedAdoptionPreview struct {
	public          AdoptionPreview
	currentContract domain.GroupModelPricingContract
	desiredContract domain.GroupModelPricingContract
	expiresAt       time.Time
}
