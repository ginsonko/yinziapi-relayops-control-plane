package mirrorservice

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorexec"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

var (
	ErrPreviewNotFound       = errors.New("mirror preview was not found or expired")
	ErrPreviewStale          = errors.New("mirror preview is stale")
	ErrEvidenceUnconfirmed   = errors.New("mirror source evidence is not confirmed")
	ErrOwnershipRequired     = errors.New("complete mirror requires one exact active channel ownership")
	ErrVisibilityUnavailable = errors.New("target marketplace, ability and cache visibility verification is unavailable")
	ErrSourceEvidencePending = errors.New("mirror source evidence is pending")
)

type sourceEvidencePendingError struct{ reason string }

func (err sourceEvidencePendingError) Error() string {
	if strings.TrimSpace(err.reason) == "" {
		return ErrSourceEvidencePending.Error()
	}
	return ErrSourceEvidencePending.Error() + ": " + strings.TrimSpace(err.reason)
}

func (sourceEvidencePendingError) Unwrap() error              { return ErrSourceEvidencePending }
func (sourceEvidencePendingError) AfterCollectDeferred() bool { return true }

func sourceEvidencePending(reason string) error {
	return sourceEvidencePendingError{reason: reason}
}

type mirrorTarget interface {
	mirrorexec.Target
	ListChannels(context.Context, domain.TargetSite) ([]domain.TargetChannel, error)
}

type Service struct {
	store         *store.Store
	targets       *targets.Factory
	resolveTarget func(context.Context, domain.TargetSite) (mirrorTarget, error)
	visibility    mirrorexec.VisibilityVerifier
	remote        *remotewrite.Gate
	options       Options

	automaticWrites  atomic.Bool
	previewMu        sync.Mutex
	previews         map[string]cachedPreview
	adoptionPreviews map[string]cachedAdoptionPreview
	refreshSupplier  func(context.Context, int64) error
	confirmationMu   sync.Mutex
	confirming       map[int64]struct{}
}

func New(st *store.Store, targetFactory *targets.Factory, visibility mirrorexec.VisibilityVerifier,
	options Options) (*Service, error) {
	if st == nil || targetFactory == nil {
		return nil, errors.New("mirror service store and target factory are required")
	}
	if options.EvidenceMaximumAge <= 0 {
		options.EvidenceMaximumAge = time.Hour
	}
	if options.PreviewTTL <= 0 {
		options.PreviewTTL = defaultPreviewTTL
	}
	if options.PreviewCapacity <= 0 {
		options.PreviewCapacity = defaultPreviewSize
	}
	if options.ProofLimit <= 0 || options.ProofLimit > 100 {
		options.ProofLimit = defaultProofLimit
	}
	if options.ConfirmationAttempts <= 0 || options.ConfirmationAttempts > 3 {
		options.ConfirmationAttempts = 2
	}
	if options.ConfirmationInterval <= 0 {
		options.ConfirmationInterval = 3 * time.Second
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	gate := targetFactory.RemoteWriteGate()
	if gate == nil {
		gate = remotewrite.New(false)
	}
	return &Service{store: st, targets: targetFactory, visibility: visibility, remote: gate,
		options: options, previews: make(map[string]cachedPreview), adoptionPreviews: make(map[string]cachedAdoptionPreview),
		confirming: make(map[int64]struct{})}, nil
}

func (s *Service) SetAutomaticWritesEnabled(enabled bool) {
	s.automaticWrites.Store(enabled)
}

func (s *Service) AutomaticWritesEnabled() bool {
	return s != nil && s.automaticWrites.Load()
}

func (s *Service) SetSupplierRefresher(refresh func(context.Context, int64) error) {
	s.confirmationMu.Lock()
	s.refreshSupplier = refresh
	s.confirmationMu.Unlock()
}

func (s *Service) ListPolicies(ctx context.Context) ([]mirror.Policy, error) {
	policies, err := s.store.ListMirrorPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for index := range policies {
		policies[index] = mirror.NormalizeOwnership(policies[index])
	}
	return policies, nil
}

// CreatePolicyDocument creates only a disabled draft. Enabling a complete
// mirror without an exact managed-channel ownership would grant destructive
// authority that has never been adopted by an administrator.
func (s *Service) CreatePolicyDocument(ctx context.Context, document mirror.PolicyDocument) (mirror.Policy, error) {
	policy, err := mirror.DecodePolicy(document)
	if err != nil {
		return mirror.Policy{}, err
	}
	if policy.Enabled {
		return mirror.Policy{}, errors.New("a new complete-mirror policy must be created disabled and adopt an exact channel before enabling")
	}
	if err := mirror.ValidatePolicy(policy, mirror.PolicyCapabilities{CompleteModelSet: true, ManagedChannelCAS: true}); err != nil {
		return mirror.Policy{}, err
	}
	if err := s.validateBindings(ctx, policy); err != nil {
		return mirror.Policy{}, err
	}
	if err := s.store.CreateMirrorPolicy(ctx, policy, s.now()); err != nil {
		return mirror.Policy{}, err
	}
	created, err := s.store.GetMirrorPolicy(ctx, policy.PolicyUUID)
	return mirror.NormalizeOwnership(created), err
}

func (s *Service) UpdatePolicyDocument(ctx context.Context, document mirror.PolicyDocument,
	expectedVersion int64) (mirror.Policy, error) {
	policy, err := mirror.DecodePolicy(document)
	if err != nil {
		return mirror.Policy{}, err
	}
	current, err := s.store.GetMirrorPolicy(ctx, policy.PolicyUUID)
	if err != nil {
		return mirror.Policy{}, err
	}
	current = mirror.NormalizeOwnership(current)
	if expectedVersion <= 0 || current.PolicyVersion != expectedVersion || policy.PolicyVersion != expectedVersion {
		return mirror.Policy{}, store.ErrMirrorPolicyVersionConflict
	}
	if current.SupplierID != policy.SupplierID || current.SourceGroup != policy.SourceGroup ||
		current.TargetSiteID != policy.TargetSiteID || current.TargetGroup != policy.TargetGroup || current.Kind != policy.Kind {
		return mirror.Policy{}, errors.New("mirror policy source and target bindings are immutable; create a new draft instead")
	}
	if err := mirror.ValidatePolicy(policy, mirror.PolicyCapabilities{CompleteModelSet: true, ManagedChannelCAS: true}); err != nil {
		return mirror.Policy{}, err
	}
	if err := s.validateBindings(ctx, policy); err != nil {
		return mirror.Policy{}, err
	}
	if policy.Enabled {
		if _, err := s.channelOwnership(ctx, policy); err != nil {
			return mirror.Policy{}, err
		}
	}
	updated, err := s.store.UpdateMirrorPolicy(ctx, policy, expectedVersion, s.now())
	return mirror.NormalizeOwnership(updated), err
}

func (s *Service) DeletePolicy(ctx context.Context, policyUUID string, expectedVersion int64) error {
	return s.store.DeleteMirrorPolicy(ctx, strings.TrimSpace(policyUUID), expectedVersion, s.now())
}

func (s *Service) validateBindings(ctx context.Context, policy mirror.Policy) error {
	if policy.Kind != mirror.PolicyKindCompleteMirror {
		return fmt.Errorf("native mirror service does not accept policy kind %q", policy.Kind)
	}
	supplier, err := s.store.GetSupplier(ctx, policy.SupplierID)
	if err != nil {
		return fmt.Errorf("read mirror supplier: %w", err)
	}
	if !supplier.Enabled {
		return errors.New("mirror supplier is disabled")
	}
	target, err := s.store.GetTarget(ctx, policy.TargetSiteID)
	if err != nil {
		return fmt.Errorf("read mirror target: %w", err)
	}
	if strings.TrimSpace(target.BaseURL) == "" {
		return errors.New("mirror target has no base URL")
	}
	return nil
}

func (s *Service) channelOwnership(ctx context.Context, policy mirror.Policy) (store.MirrorChannelOwnership, error) {
	items, err := s.store.ListActiveMirrorChannelOwnerships(ctx, policy.PolicyUUID)
	if err != nil {
		return store.MirrorChannelOwnership{}, err
	}
	if len(items) != 1 {
		return store.MirrorChannelOwnership{}, ErrOwnershipRequired
	}
	item := items[0]
	marker := "relayops:policy/" + policy.PolicyUUID
	if item.PolicyUUID != policy.PolicyUUID || item.TargetSiteID != policy.TargetSiteID ||
		item.TargetGroup != policy.TargetGroup || item.ChannelID <= 0 || item.OwnershipMarker != marker || !item.Active {
		return store.MirrorChannelOwnership{}, ErrOwnershipRequired
	}
	return item, nil
}

func (s *Service) target(ctx context.Context, targetSite domain.TargetSite) (mirrorTarget, error) {
	if s.resolveTarget != nil {
		return s.resolveTarget(ctx, targetSite)
	}
	adapter, err := s.targets.ForTarget(targetSite)
	if err != nil {
		return nil, err
	}
	result, ok := adapter.(mirrorTarget)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not implement the complete-mirror contract", adapter.Name())
	}
	return result, nil
}

func (s *Service) now() time.Time { return s.options.Now().UTC() }

func (s *Service) remember(entry cachedPreview) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	now := s.now()
	for id, candidate := range s.previews {
		if !candidate.expiresAt.After(now) {
			delete(s.previews, id)
		}
	}
	if len(s.previews) >= s.options.PreviewCapacity {
		type pair struct {
			id string
			at time.Time
		}
		ordered := make([]pair, 0, len(s.previews))
		for id, candidate := range s.previews {
			ordered = append(ordered, pair{id: id, at: candidate.expiresAt})
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].at.Before(ordered[j].at) })
		delete(s.previews, ordered[0].id)
	}
	s.previews[entry.public.ID] = entry
}

func (s *Service) cached(id string) (cachedPreview, error) {
	id = strings.TrimSpace(id)
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	entry, ok := s.previews[id]
	if !ok || !entry.expiresAt.After(s.now()) {
		delete(s.previews, id)
		return cachedPreview{}, ErrPreviewNotFound
	}
	return entry, nil
}
