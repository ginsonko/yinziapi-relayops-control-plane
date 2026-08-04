package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

const (
	browserCustodyHeader               = "X-RelayOps-Custody"
	browserCustodyHelperVersionHeader  = "X-RelayOps-Helper-Version"
	browserCustodyMinimumHelperVersion = "1.3.0"
	browserCustodyHeartbeatTTL         = 6 * time.Minute
	browserCustodyRefreshAhead         = 5 * time.Minute
	browserCustodyRecoveryGrace        = 10 * time.Minute
)

type browserCustodyCaptureInput struct {
	Payload domain.CredentialPayload `json:"payload"`
}

func (s *Server) handleBrowserCustodyOptions(w http.ResponseWriter, _ *http.Request) {
	setBrowserCustodyCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func setBrowserCustodyCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-RelayOps-Custody, X-RelayOps-Helper-Version")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.Header().Set("Cache-Control", "no-store")
}

func custodyTokenHash(r *http.Request) (string, bool) {
	token := strings.TrimSpace(r.Header.Get(browserCustodyHeader))
	if token == "" {
		return "", false
	}
	return tokenHash(token), true
}

func (s *Server) handleBrowserCustodyPublicStatus(w http.ResponseWriter, r *http.Request) {
	setBrowserCustodyCORS(w)
	tokenDigest, ok := custodyTokenHash(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "browser custody token is required")
		return
	}
	now := time.Now().UTC()
	binding, err := s.store.GetActiveBrowserCustodyByToken(r.Context(), tokenDigest, now)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "browser custody token is invalid, expired, or revoked")
		return
	}
	helperVersion := browserCustodyHelperVersion(r)
	_ = s.store.RecordBrowserCustodyCheck(r.Context(), tokenDigest, helperVersion, now)
	binding.HelperVersion = helperVersion
	binding.LastCheckAt = &now
	status := s.browserCustodyStatus(r.Context(), binding, now)
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleBrowserCustodyCapture(w http.ResponseWriter, r *http.Request) {
	setBrowserCustodyCORS(w)
	tokenDigest, ok := custodyTokenHash(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "browser custody token is required")
		return
	}
	now := time.Now().UTC()
	binding, err := s.store.GetActiveBrowserCustodyByToken(r.Context(), tokenDigest, now)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "browser custody token is invalid, expired, or revoked")
		return
	}
	if helperVersion := browserCustodyHelperVersion(r); !browserCustodyVersionAtLeast(helperVersion, browserCustodyMinimumHelperVersion) {
		writeJSON(w, http.StatusUpgradeRequired, map[string]any{"message": "login helper upgrade is required before browser custody can replace credentials",
			"minimum_helper_version": browserCustodyMinimumHelperVersion})
		return
	}
	lock := s.browserCustodyLock(binding.SupplierID)
	lock.Lock()
	defer lock.Unlock()

	// Re-read after entering the supplier lock. Revocation, re-pairing and a
	// concurrent successful capture must all win over this stale request.
	binding, err = s.store.GetActiveBrowserCustodyByToken(r.Context(), tokenDigest, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusConflict, "browser custody binding changed; the older candidate was not saved")
		return
	}
	status := s.browserCustodyStatus(r.Context(), binding, time.Now().UTC())
	if !status.NeedsCapture {
		writeJSON(w, http.StatusOK, map[string]any{"captured": false, "reason": "current credential remains healthy"})
		return
	}
	var input browserCustodyCaptureInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "browser custody payload is invalid")
		return
	}
	if strings.TrimSpace(input.Payload.CapturedOrigin) == "" {
		s.recordBrowserCustodyFailure(r.Context(), binding, "origin_missing", "The opened upstream page did not report its origin.")
		writeError(w, http.StatusBadRequest, "captured browser origin is required")
		return
	}
	if err := netguard.RequireSameOrigin(input.Payload.CapturedOrigin, binding.ExpectedOrigin); err != nil {
		s.recordBrowserCustodyFailure(r.Context(), binding, "origin_mismatch", "The opened page does not match the scoped upstream origin.")
		writeError(w, http.StatusForbidden, "captured browser origin does not match this custody binding")
		return
	}
	if !hasCredentialMaterial(input.Payload) {
		s.recordBrowserCustodyFailure(r.Context(), binding, "credential_missing", "The opened upstream page has no reusable login session yet.")
		writeError(w, http.StatusBadRequest, "the opened upstream page has no reusable login session")
		return
	}
	supplier, err := s.store.GetSupplier(r.Context(), binding.SupplierID)
	if err != nil || supplier.CredentialID == nil || *supplier.CredentialID != binding.CredentialID {
		writeError(w, http.StatusConflict, "supplier credential changed; re-pair the current browser session")
		return
	}
	credential, existing, err := s.credentials.Load(r.Context(), binding.CredentialID)
	if err != nil {
		writeError(w, http.StatusConflict, "the scoped credential no longer exists")
		return
	}
	candidate := normalizeCredential(input.Payload)
	diagnostics, validationErr := s.collector.ValidateCredential(r.Context(), supplier, credential.Kind, candidate)
	if validationErr != nil && s.refresher != nil {
		errorClass := connectors.ErrorClass(validationErr)
		if errorClass == "auth_expired" || errorClass == "auth_missing" {
			if refreshed, _, refreshErr := s.refresher.RefreshCapturedCandidate(r.Context(), supplier, candidate); refreshErr == nil {
				candidate = refreshed
				diagnostics, validationErr = s.collector.ValidateCredential(r.Context(), supplier, credential.Kind, candidate)
			} else if errors.Is(refreshErr, remotewrite.ErrDisabled) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": remotewrite.DisabledMessage,
					"error_class": "remote_writes_disabled", "retryable": false, "attempt_consumed": false,
					"old_credential_preserved": true})
				return
			}
		}
	}
	if validationErr != nil {
		errorClass := connectors.ErrorClass(validationErr)
		message := pairingValidationMessage(errorClass)
		s.recordBrowserCustodyFailure(r.Context(), binding, errorClass, message)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": message, "error_class": errorClass,
			"diagnostics": diagnostics, "old_credential_preserved": true})
		return
	}
	renewal, _ := s.store.GetCredentialRenewal(r.Context(), credential.ID)
	renewal.ConsecutiveFailures = 0
	renewal.LastAttemptAt = &now
	renewal.LastSuccessAt = &now
	renewal.HumanAction = ""
	renewal.LastErrorClass = ""
	if renewal.Configured {
		renewal.State = "healthy"
		if renewal.Enabled && existing.Login != nil {
			next := now.Add(time.Duration(existing.Login.MinimumIntervalSeconds) * time.Second)
			renewal.NextAttemptAt = &next
		} else {
			renewal.NextAttemptAt = nil
		}
	} else {
		renewal.State = "unconfigured"
		renewal.NextAttemptAt = nil
	}
	if err := s.credentials.CommitBrowserCustodyRenewal(r.Context(), credential, existing, candidate, renewal,
		tokenDigest, supplier.ID); err != nil {
		if errors.Is(err, store.ErrCredentialChanged) {
			s.recordBrowserCustodyFailure(r.Context(), binding, "concurrent_change", "A newer manual credential was preserved.")
			writeError(w, http.StatusConflict, "credential changed during validation; the newer value was preserved")
			return
		}
		writeError(w, http.StatusConflict, "verified browser session could not be committed; the old credential was preserved")
		return
	}
	supplierID := supplier.ID
	_ = s.store.AddActivityCoalesced(r.Context(), domain.Activity{Kind: "browser_custody_recovered", Severity: "success",
		Title: "浏览器托管已恢复上游授权", Message: supplier.Name + " 的新会话通过只读能力验证并已原子替换",
		SupplierID: &supplierID, Metadata: map[string]string{"supplier_id": fmt.Sprint(supplier.ID), "supplier_name": supplier.Name}}, 24*time.Hour)
	go func() {
		ctx, cancel := background(90 * time.Second)
		defer cancel()
		_, _ = s.collector.CollectOrJoin(ctx, supplier.ID)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"captured": true, "collection_started": true,
		"validation": diagnostics, "old_credential_preserved": false})
}

func (s *Server) handleBrowserCustodySelfRevoke(w http.ResponseWriter, r *http.Request) {
	setBrowserCustodyCORS(w)
	tokenDigest, ok := custodyTokenHash(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "browser custody token is required")
		return
	}
	if err := s.store.RevokeBrowserCustodyByToken(r.Context(), tokenDigest, time.Now().UTC()); err != nil {
		writeError(w, http.StatusUnauthorized, "browser custody token is invalid or already revoked")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (s *Server) handleBrowserCustodyAdminStatus(w http.ResponseWriter, r *http.Request) {
	supplierID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	binding, err := s.store.GetBrowserCustodyForSupplier(r.Context(), supplierID, now)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, domain.BrowserCustodyStatus{SupplierID: supplierID})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read browser custody status")
		return
	}
	writeJSON(w, http.StatusOK, s.browserCustodyStatus(r.Context(), binding, now))
}

func (s *Server) handleBrowserCustodyAdminRevoke(w http.ResponseWriter, r *http.Request) {
	supplierID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.RevokeBrowserCustodyBySupplier(r.Context(), supplierID, time.Now().UTC()); err != nil {
		writeError(w, http.StatusNotFound, "this supplier has no active browser custody binding")
		return
	}
	_ = s.store.AddActivity(r.Context(), domain.Activity{Kind: "browser_custody_revoked", Severity: "info",
		Title: "浏览器托管已撤销", Message: "扩展将不再读取该上游的已打开页面", SupplierID: &supplierID})
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "supplier_id": supplierID})
}

func (s *Server) browserCustodyLock(supplierID int64) *sync.Mutex {
	value, _ := s.browserCustodyLocks.LoadOrStore(supplierID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (s *Server) browserCustodyStatus(ctx context.Context, binding store.BrowserCustodyBinding,
	now time.Time) domain.BrowserCustodyStatus {
	status := binding.BrowserCustodyStatus
	status.MinimumVersion = browserCustodyMinimumHelperVersion
	status.HelperState = "binding_pending"
	if status.LastCheckAt != nil {
		if now.Sub(*status.LastCheckAt) > browserCustodyHeartbeatTTL {
			status.HelperState = "offline"
		} else if !browserCustodyVersionAtLeast(status.HelperVersion, browserCustodyMinimumHelperVersion) {
			status.HelperState = "upgrade_required"
		} else {
			status.HelperState = "online"
			status.HelperOnline = true
		}
	}
	if !status.Enabled {
		status.HelperState = "disabled"
		status.HelperOnline = false
		status.NeedReason = "disabled"
		return status
	}
	supplier, err := s.store.GetSupplier(ctx, status.SupplierID)
	if err != nil || supplier.CredentialID == nil || *supplier.CredentialID != status.CredentialID {
		status.NeedsCapture = false
		status.NeedReason = "credential_changed"
		return status
	}
	credential, _, err := s.store.GetCredentialEncrypted(ctx, status.CredentialID)
	if err != nil {
		status.NeedReason = "credential_missing"
		return status
	}
	cutoff := time.Time{}
	if status.LastSuccessAt != nil {
		cutoff = *status.LastSuccessAt
	}
	capabilities, _ := s.store.ListAuthCapabilities(ctx, status.SupplierID)
	type coreEvidence struct {
		verified time.Time
		failed   time.Time
	}
	byDimension := map[string]coreEvidence{}
	latestCoreAttempt := time.Time{}
	for _, capability := range capabilities {
		// Anonymous/public probes cannot prove or disprove the scoped browser
		// session. Only evidence explicitly attached to this credential counts.
		if capability.CredentialID == nil || *capability.CredentialID != status.CredentialID {
			continue
		}
		dimension := browserCustodyCoreDimension(capability.EndpointType)
		if dimension == "" {
			continue
		}
		state := byDimension[dimension]
		if capability.LastAttemptAt.After(latestCoreAttempt) {
			latestCoreAttempt = capability.LastAttemptAt
		}
		if capability.Status == "verified" {
			at := capability.LastAttemptAt
			if capability.VerifiedAt != nil && capability.VerifiedAt.After(at) {
				at = *capability.VerifiedAt
			}
			if at.After(state.verified) {
				state.verified = at
			}
		}
		if isAuthFailureClass(capability.ErrorClass) && capability.LastAttemptAt.After(state.failed) {
			state.failed = capability.LastAttemptAt
		}
		byDimension[dimension] = state
	}
	for _, dimension := range []string{"catalog", "balance"} {
		evidence := byDimension[dimension]
		if evidence.failed.After(cutoff) && !evidence.failed.Before(evidence.verified) {
			markBrowserCustodyRecovery(&status, dimension+"_auth_expired", evidence.failed, now)
			return status
		}
	}
	if isAuthFailureClass(supplier.LastErrorClass) && supplier.LastAttemptAt != nil &&
		supplier.LastAttemptAt.After(cutoff) && supplier.LastAttemptAt.After(latestCoreAttempt) {
		markBrowserCustodyRecovery(&status, "supplier_auth_expired", *supplier.LastAttemptAt, now)
		return status
	}
	if isAuthFailureClass(credential.Status) && credential.UpdatedAt.After(cutoff) {
		markBrowserCustodyRecovery(&status, "credential_auth_expired", credential.UpdatedAt, now)
		return status
	}
	renewal, _ := s.store.GetCredentialRenewal(ctx, credential.ID)
	renewalEvidenceAt := renewal.UpdatedAt
	if renewal.LastAttemptAt != nil && (renewalEvidenceAt == nil || renewal.LastAttemptAt.After(*renewalEvidenceAt)) {
		renewalEvidenceAt = renewal.LastAttemptAt
	}
	if (renewal.State == "human_action_required" || renewal.State == "auth_expired") && renewalEvidenceAt != nil &&
		renewalEvidenceAt.After(cutoff) {
		markBrowserCustodyRecovery(&status, "browser_human_action_required", *renewalEvidenceAt, now)
		return status
	}
	if needSince, needed := browserCustodyExpiryDemand(credential.ExpiresAt, now); needed {
		deadline := credential.ExpiresAt.UTC().Add(browserCustodyRecoveryGrace)
		markBrowserCustodyRecoveryUntil(&status, "credential_expiring", needSince, deadline, now)
		return status
	}
	status.NeedReason = "healthy"
	return status
}

func browserCustodyExpiryDemand(expiresAt *time.Time, now time.Time) (time.Time, bool) {
	if expiresAt == nil || expiresAt.IsZero() || expiresAt.After(now.Add(browserCustodyRefreshAhead)) {
		return time.Time{}, false
	}
	return expiresAt.UTC().Add(-browserCustodyRefreshAhead), true
}

func markBrowserCustodyRecovery(status *domain.BrowserCustodyStatus, reason string, since, now time.Time) {
	markBrowserCustodyRecoveryUntil(status, reason, since, since.Add(browserCustodyRecoveryGrace), now)
}

func markBrowserCustodyRecoveryUntil(status *domain.BrowserCustodyStatus, reason string, since, deadline, now time.Time) {
	if since.IsZero() || since.After(now) {
		since = now
	}
	since = since.UTC()
	status.NeedsCapture = true
	status.NeedReason = reason
	status.NeedSince = &since
	rejected := strings.TrimSpace(status.LastErrorClass) != "" && status.LastSubmissionAt != nil &&
		!status.LastSubmissionAt.Before(since)
	status.RecoveryPending = status.HelperOnline && !rejected && now.Before(deadline.UTC())
	status.RecoveryOverdue = !status.RecoveryPending
}

func browserCustodyHelperVersion(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get(browserCustodyHelperVersionHeader))
	if len(value) > 32 {
		return ""
	}
	for _, character := range value {
		if (character < '0' || character > '9') && character != '.' {
			return ""
		}
	}
	return value
}

func browserCustodyVersionAtLeast(current, minimum string) bool {
	parse := func(value string) ([3]int, bool) {
		var output [3]int
		parts := strings.Split(strings.TrimSpace(value), ".")
		if len(parts) != len(output) {
			return output, false
		}
		for index, part := range parts {
			parsed, err := strconv.Atoi(part)
			if err != nil || parsed < 0 {
				return output, false
			}
			output[index] = parsed
		}
		return output, true
	}
	actual, actualOK := parse(current)
	required, requiredOK := parse(minimum)
	if !actualOK || !requiredOK {
		return false
	}
	for index := range actual {
		if actual[index] != required[index] {
			return actual[index] > required[index]
		}
	}
	return true
}

func browserCustodyCoreDimension(endpoint string) string {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "catalog", "pricing", "public_catalog", "customer_catalog", "customer_key_catalog":
		return "catalog"
	case "balance", "customer_balance":
		return "balance"
	default:
		return ""
	}
}

func isAuthFailureClass(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auth_expired", "auth_missing", "expired_or_invalid":
		return true
	default:
		return false
	}
}

func (s *Server) recordBrowserCustodyFailure(ctx context.Context, binding store.BrowserCustodyBinding,
	errorClass, safeMessage string) {
	now := time.Now().UTC()
	_ = s.store.RecordBrowserCustodyFailure(ctx, binding.TokenHash, errorClass, safeMessage, now)
	supplierID := binding.SupplierID
	_ = s.store.AddActivity(ctx, domain.Activity{Kind: "browser_custody_capture_failed", Severity: "warning",
		Title: "浏览器托管候选未通过验证", Message: safeMessage, SupplierID: &supplierID,
		Metadata: map[string]string{"error_class": errorClass}})
}
