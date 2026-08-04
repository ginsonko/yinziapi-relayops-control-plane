package httpapi

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/automation"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/collector"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ledgersync"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/listing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type Server struct {
	store                    *store.Store
	startedAt                time.Time
	credentials              *credentials.Service
	refresher                *credentials.Refresher
	collector                *collector.Service
	automation               *automation.Service
	mirror                   mirrorServiceAPI
	listing                  *listing.Service
	intelligence             *intelligence.Service
	ledgerSync               *ledgersync.Service
	targets                  *targets.Factory
	httpClient               *http.Client
	staticDir                string
	adminToken               string
	runwayHours              decimal.Decimal
	credentialRefreshAhead   time.Duration
	automationEnabled        atomic.Bool
	automationDefaultEnabled bool
	backgroundWorkersEnabled bool
	remoteWrites             *remotewrite.Gate
	pairingMaxAttempts       int
	pairingTransientCooldown time.Duration
	collectionJobsMu         sync.RWMutex
	collectionJobs           map[string]collectionJob
	activeCollectionJobID    string
	browserCustodyLocks      sync.Map
	pairingValidationLocks   sync.Map
	overviewWatchMu          sync.RWMutex
	overviewWatchItems       []consoleWatch
	overviewWatchSettings    store.WatchSettings
	overviewWatchCandidates  int
}

const (
	apiContractHeader     = "X-RelayOps-API-Contract"
	apiContractVersion    = "relayops-v3-r49-4-settings-busy-retry-20260804"
	requestContextTimeout = 4*time.Minute + 30*time.Second
)

type Dependencies struct {
	Store                    *store.Store
	Credentials              *credentials.Service
	Refresher                *credentials.Refresher
	Collector                *collector.Service
	Automation               *automation.Service
	Mirror                   mirrorServiceAPI
	Listing                  *listing.Service
	LedgerSync               *ledgersync.Service
	Targets                  *targets.Factory
	HTTPClient               *http.Client
	StaticDir                string
	AdminToken               string
	RunwayHours              int
	CredentialRefreshAhead   time.Duration
	AutomationEnabled        bool
	BackgroundWorkersEnabled bool
	RemoteWriteGate          *remotewrite.Gate
	PairingMaximumAttempts   int
	PairingTransientCooldown time.Duration
}

func New(deps Dependencies) http.Handler {
	remoteWriteGate := deps.RemoteWriteGate
	if remoteWriteGate == nil {
		remoteWriteGate = remotewrite.New(true)
	}
	effectiveAutomationEnabled := deps.AutomationEnabled && remoteWriteGate.Enabled()
	if deps.Targets != nil {
		deps.Targets.SetRemoteWriteGate(remoteWriteGate)
	}
	if deps.Automation != nil {
		deps.Automation.SetRemoteWriteGate(remoteWriteGate)
	}
	if deps.Listing != nil {
		deps.Listing.SetRemoteWriteGate(remoteWriteGate)
	}
	if deps.Refresher != nil {
		deps.Refresher.WithRemoteWriteGate(remoteWriteGate)
	}
	pairingMaxAttempts := deps.PairingMaximumAttempts
	if pairingMaxAttempts <= 0 {
		pairingMaxAttempts = 3
	}
	pairingTransientCooldown := deps.PairingTransientCooldown
	if pairingTransientCooldown <= 0 {
		pairingTransientCooldown = 10 * time.Second
	}
	credentialRefreshAhead := deps.CredentialRefreshAhead
	if credentialRefreshAhead <= 0 {
		credentialRefreshAhead = 15 * time.Minute
	}
	server := &Server{store: deps.Store, startedAt: time.Now().UTC(), credentials: deps.Credentials, refresher: deps.Refresher, collector: deps.Collector,
		automation: deps.Automation, mirror: deps.Mirror, listing: deps.Listing, intelligence: intelligence.NewService(deps.Store), ledgerSync: deps.LedgerSync,
		targets: deps.Targets, httpClient: deps.HTTPClient, staticDir: deps.StaticDir,
		adminToken: strings.TrimSpace(deps.AdminToken), runwayHours: decimal.NewFromInt(int64(deps.RunwayHours)),
		credentialRefreshAhead:   credentialRefreshAhead,
		automationDefaultEnabled: deps.AutomationEnabled,
		backgroundWorkersEnabled: deps.BackgroundWorkersEnabled, pairingMaxAttempts: pairingMaxAttempts,
		pairingTransientCooldown: pairingTransientCooldown, remoteWrites: remoteWriteGate}
	server.collectionJobs = make(map[string]collectionJob)
	server.setAutomaticWritesEnabled(effectiveAutomationEnabled)
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Timeout(requestContextTimeout))
	router.Use(server.securityHeaders)
	router.Get("/health", server.handleHealth)
	router.Route("/api/v1", func(api chi.Router) {
		api.Get("/health", server.handleHealth)
		api.Options("/pair/{token}", server.handlePairOptions)
		api.Post("/pair/{token}", server.handlePairComplete)
		api.Get("/pair/{token}/status", server.handlePairStatus)
		api.Options("/browser-custody", server.handleBrowserCustodyOptions)
		api.Options("/browser-custody/status", server.handleBrowserCustodyOptions)
		api.Options("/browser-custody/capture", server.handleBrowserCustodyOptions)
		api.Get("/browser-custody/status", server.handleBrowserCustodyPublicStatus)
		api.Post("/browser-custody/capture", server.handleBrowserCustodyCapture)
		api.Delete("/browser-custody", server.handleBrowserCustodySelfRevoke)
		api.Group(func(protected chi.Router) {
			protected.Use(server.requireAdmin)
			protected.Use(server.requireCompatibleAPIContract)
			protected.Get("/console/bootstrap", server.handleBootstrap)
			protected.Get("/watchlist", server.handleWatchlist)
			protected.Post("/watchlist/items", server.handleCreateWatchItem)
			protected.Put("/watchlist/items/{id}", server.handleUpdateWatchItem)
			protected.Delete("/watchlist/items/{id}", server.handleDeleteWatchItem)
			protected.Put("/watchlist/order", server.handleWatchOrder)
			protected.Get("/watchlist/settings", server.handleWatchSettings)
			protected.Put("/watchlist/settings", server.handleWatchSettings)
			protected.Get("/activities", server.handleListActivities)
			protected.Post("/activities/mirror-draft-batch", server.handleRecordMirrorDraftBatch)
			protected.Delete("/activities", server.handleDeleteActivities)
			protected.Get("/automation-control", server.handleAutomationControl)
			protected.Put("/automation-control", server.handleUpdateAutomationControl)
			protected.Get("/automation-confirmation", server.handleAutomationConfirmation)
			protected.Put("/automation-confirmation", server.handleUpdateAutomationConfirmation)
			protected.Get("/automation-safety", server.handleAutomationSafety)
			protected.Put("/automation-safety", server.handleUpdateAutomationSafety)
			protected.Get("/suppliers", server.handleListSuppliers)
			protected.Post("/suppliers", server.handleCreateSupplier)
			protected.Put("/suppliers/{id}", server.handleUpdateSupplier)
			protected.Get("/suppliers/{id}/deletion-impact", server.handleSupplierDeletionImpact)
			protected.Delete("/suppliers/{id}", server.handleDeleteSupplier)
			protected.Post("/suppliers/collect", server.handleCollectAll)
			protected.Get("/collection-jobs/{id}", server.handleCollectionJob)
			protected.Post("/suppliers/{id}/collect", server.handleCollectSupplier)
			protected.Get("/suppliers/{id}/snapshot", server.handleSnapshot)
			protected.Get("/suppliers/{id}/prices", server.handlePrices)
			protected.Get("/suppliers/{id}/attempts", server.handleCollectionAttempts)
			protected.Get("/suppliers/{id}/capabilities", server.handleSupplierCapabilities)
			protected.Get("/suppliers/{id}/credential-renewal-preset", server.handleCredentialRenewalPreset)
			protected.Post("/suppliers/{id}/login", server.handleSupplierPasswordLogin)
			protected.Get("/suppliers/{id}/browser-custody", server.handleBrowserCustodyAdminStatus)
			protected.Delete("/suppliers/{id}/browser-custody", server.handleBrowserCustodyAdminRevoke)
			protected.Get("/credentials", server.handleListCredentials)
			protected.Post("/credentials", server.handleCreateCredential)
			protected.Get("/credentials/{id}/deletion-impact", server.handleCredentialDeletionImpact)
			protected.Delete("/credentials/{id}", server.handleDeleteCredential)
			protected.Post("/credentials/{id}/refresh", server.handleRefreshCredential)
			protected.Get("/credentials/{id}/renewal", server.handleCredentialRenewalStatus)
			protected.Put("/credentials/{id}/renewal", server.handleConfigureCredentialRenewal)
			protected.Delete("/credentials/{id}/renewal", server.handleRemoveCredentialRenewal)
			protected.Post("/credentials/{id}/renewal/trigger", server.handleTriggerCredentialRenewal)
			protected.Post("/credentials/{supplierID}/pair", server.handlePairStart)
			protected.Get("/targets", server.handleListTargets)
			protected.Post("/targets/connect", server.handleConnectTarget)
			protected.Post("/targets", server.handleCreateTarget)
			protected.Put("/targets/{id}", server.handleUpdateTarget)
			protected.Get("/targets/{id}/deletion-impact", server.handleTargetDeletionImpact)
			protected.Delete("/targets/{id}", server.handleDeleteTarget)
			protected.Post("/targets/{id}/test", server.handleTestTarget)
			protected.Get("/targets/{id}/ledger-sync", server.handleTargetLedgerSyncStatus)
			protected.Put("/targets/{id}/ledger-sync", server.handleTargetLedgerSyncConfig)
			protected.Post("/targets/{id}/ledger-sync/run", server.handleRunTargetLedgerSync)
			protected.Post("/targets/{id}/reauthorize", server.handleReauthorizeTarget)
			protected.Get("/targets/{id}/capabilities", server.handleTargetCapabilities)
			protected.Get("/targets/{id}/operations", server.handleListTargetControlOperations)
			protected.Get("/targets/{id}/catalog", server.handleTargetCatalog)
			protected.Get("/targets/{id}/channels", server.handleTargetChannels)
			protected.Put("/targets/{id}/group-ratios", server.handleApplyTargetGroupRatios)
			protected.Post("/targets/{id}/pricing-options/{option}/preview", server.handlePreviewTargetPricingOption)
			protected.Post("/targets/{id}/pricing-options/{option}/apply", server.handleApplyTargetPricingOption)
			protected.Post("/targets/{id}/channels/{channelID}/routing/preview", server.handlePreviewTargetChannelRouting)
			protected.Post("/targets/{id}/channels/{channelID}/routing/apply", server.handleApplyTargetChannelRouting)
			protected.Post("/targets/{id}/channels/{channelID}/status/preview", server.handlePreviewTargetChannelStatus)
			protected.Post("/targets/{id}/channels/{channelID}/status/apply", server.handleApplyTargetChannelStatus)
			protected.Post("/targets/{id}/channels/{channelID}/test", server.handleControlledTargetChannelTest)
			protected.Get("/pricing-policies", server.handleListPolicies)
			protected.Post("/pricing-policies", server.handleCreatePolicy)
			protected.Post("/pricing-policies/bulk/preview", server.handlePreviewPolicyBulkMutation)
			protected.Post("/pricing-policies/bulk/apply", server.handleApplyPolicyBulkMutation)
			protected.Put("/pricing-policies/{id}", server.handleUpdatePolicy)
			protected.Patch("/pricing-policies/{id}", server.handlePatchPolicy)
			protected.Get("/pricing-policies/{id}/deletion-impact", server.handlePricingPolicyDeletionImpact)
			protected.Delete("/pricing-policies/{id}", server.handleDeletePricingPolicy)
			protected.Post("/pricing-policies/{id}/simulate", server.handleSimulatePolicy)
			protected.Post("/pricing-policies/{id}/propose", server.handleProposePolicy)
			protected.Post("/mapping-drafts/preview", server.handlePreviewMappingDrafts)
			protected.Post("/mapping-drafts/materialize", server.handleMaterializeMappingDrafts)
			protected.Post("/mapping-drafts/manual/preview", server.handlePreviewManualMappingDrafts)
			protected.Post("/mapping-drafts/manual/materialize", server.handleMaterializeManualMappingDrafts)
			protected.Get("/change-proposals", server.handleListProposals)
			protected.Post("/change-proposals/{id}/apply", server.handleApplyProposal)
			protected.Post("/change-proposals/{id}/reject", server.handleRejectProposal)
			protected.Post("/change-proposals/{id}/rollback", server.handleRollbackProposal)
			protected.Post("/listing-plans/preview", server.handlePreviewListingPlan)
			protected.Get("/listing-plans", server.handleListListingPlans)
			protected.Get("/listing-plans/{id}", server.handleGetListingPlan)
			protected.Post("/listing-plans/{id}/apply", server.handleApplyListingPlan)
			protected.Get("/listing-change-sets", server.handleListListingChangeSets)
			protected.Get("/listing-change-sets/{id}", server.handleGetListingChangeSet)
			protected.Post("/listing-change-sets/{id}/rollback", server.handleRollbackListingChangeSet)
			protected.Get("/mirror-policies", server.handleListMirrorPolicies)
			protected.Post("/mirror-policies", server.handleCreateMirrorPolicy)
			protected.Put("/mirror-policies/{uuid}", server.handleUpdateMirrorPolicy)
			protected.Delete("/mirror-policies/{uuid}", server.handleDeleteMirrorPolicy)
			protected.Post("/mirror-policies/{uuid}/preview", server.handlePreviewMirrorPolicy)
			protected.Get("/mirror-previews/{id}", server.handleGetMirrorPreview)
			protected.Post("/mirror-previews/{id}/apply", server.handleApplyMirrorPreview)
			protected.Post("/mirror-adoptions/preview", server.handlePreviewMirrorAdoption)
			protected.Get("/mirror-adoption-previews/{id}", server.handleGetMirrorAdoptionPreview)
			protected.Post("/mirror-adoption-previews/{id}/apply", server.handleApplyMirrorAdoption)
			protected.Get("/mirror-change-sets", server.handleListMirrorChangeSets)
			protected.Get("/mirror-change-sets/{id}", server.handleGetMirrorChangeSet)
			protected.Get("/mirror-change-sets/{id}/events", server.handleListMirrorChangeEvents)
			protected.Get("/discoveries", server.handleDiscoveries)
			protected.Post("/discoveries/acknowledge", server.handleAcknowledgeDiscovery)
			protected.Get("/opportunities", server.handleOpportunities)
			protected.Get("/opportunities/config", server.handleOpportunityConfig)
			protected.Put("/opportunities/config", server.handleOpportunityConfig)
			protected.Put("/opportunities/usage", server.handleOpportunityUsage)
			protected.Get("/intelligence", server.handleIntelligence)
			protected.Get("/intelligence/overview", server.handleOverviewIntelligence)
			protected.Get("/intelligence/config", server.handleIntelligenceConfig)
			protected.Put("/intelligence/config", server.handleIntelligenceConfig)
			protected.Put("/intelligence/ledger", server.handleIntelligenceLedger)
			protected.Get("/connector-specs", server.handleConnectorSpecs)
			protected.Post("/connector-specs", server.handleCreateConnectorSpec)
			protected.Put("/connector-specs/{id}", server.handleUpdateConnectorSpec)
			protected.Get("/connector-specs/{id}/deletion-impact", server.handleConnectorSpecDeletionImpact)
			protected.Delete("/connector-specs/{id}", server.handleDeleteConnectorSpec)
			protected.Get("/connector-manifests", server.handleListConnectorManifests)
			protected.Post("/connector-manifests", server.handleImportConnectorManifest)
			protected.Put("/connector-manifests/{id}", server.handleUpdateConnectorManifest)
			protected.Get("/connector-manifests/{id}/deletion-impact", server.handleConnectorManifestDeletionImpact)
			protected.Delete("/connector-manifests/{id}", server.handleDeleteConnectorManifest)
			protected.Get("/connector-manifests/{id}/export", server.handleExportConnectorManifest)
			protected.Get("/suppliers/{id}/connector-manifest", server.handleGetSupplierConnectorManifest)
			protected.Put("/suppliers/{id}/connector-manifest", server.handleBindSupplierConnectorManifest)
			server.registerConfigPortabilityRoutes(protected)
		})
	})
	router.Handle("/*", server.staticHandler())
	return router
}

func (s *Server) setAutomaticWritesEnabled(enabled bool) {
	if s.automation != nil {
		s.automation.SetAutomaticWritesEnabled(enabled)
	}
	if s.mirror != nil {
		s.mirror.SetAutomaticWritesEnabled(enabled)
	}
	s.automationEnabled.Store(enabled)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set(apiContractHeader, apiContractVersion)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireCompatibleAPIContract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimSpace(r.Header.Get(apiContractHeader))
		if provided != "" && provided != apiContractVersion {
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"控制台版本已更新：当前页面使用 %s，服务端使用 %s。请重新加载后再操作", provided, apiContractVersion))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	if s.adminToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimSpace(r.Header.Get("X-RelayOps-Token"))
		if provided == "" {
			provided = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		if len(provided) != len(s.adminToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.adminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "需要管理令牌")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "relayops", "api_contract_version": apiContractVersion,
		"remote_writes_enabled": s.remoteWrites.Enabled(), "time": time.Now().UTC()})
}

func (s *Server) requireRemoteWrites(w http.ResponseWriter, action string) bool {
	if err := s.remoteWrites.Check(action); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return false
	}
	return true
}

func (s *Server) staticHandler() http.Handler {
	index := filepath.Join(s.staticDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"service": "RelayOps", "message": "管理前端尚未构建，请运行 web 构建命令"})
		})
	}
	root := os.DirFS(s.staticDir)
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(r.URL.Path)), "/")
		if path != "." && path != "" {
			if _, err := fs.Stat(root, path); err == nil {
				if path == "index.html" {
					w.Header().Set("Cache-Control", "no-store")
				} else {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, index)
	})
}

func parseID(r *http.Request, key string) (int64, error) {
	value, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return value, nil
}

func readJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": status < 400, "data": value})
}

func writeCompressedJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		writeJSON(w, status, value)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Encoding", "gzip")
	writer, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		w.Header().Del("Content-Encoding")
		writeJSON(w, status, value)
		return
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": status < 400, "data": value})
	_ = writer.Close()
}

func acceptsGzip(raw string) bool {
	for _, item := range strings.Split(raw, ",") {
		parts := strings.Split(item, ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		for _, parameter := range parts[1:] {
			key, value, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			quality, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || quality <= 0 {
				return false
			}
		}
		return true
	}
	return false
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "message": message})
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func background(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

func statusCode(err error) int {
	if errors.Is(err, remotewrite.ErrDisabled) {
		return http.StatusServiceUnavailable
	}
	if store.IsNotFound(err) {
		return http.StatusNotFound
	}
	if errors.Is(err, store.ErrSupplierDeletionConflict) ||
		errors.Is(err, store.ErrSupplierDeletionAuthorityRetentionRequired) {
		return http.StatusConflict
	}
	if errors.Is(err, store.ErrTargetDeletionConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, store.ErrPricingPolicyDeletionConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, store.ErrCredentialDeletionConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, store.ErrConnectorSpecConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, store.ErrConnectorManifestConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, listing.ErrRollbackConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadRequest
}

func (s *Server) validateCredentialOrigin(ctx context.Context, credentialID int64, destination string) error {
	if _, err := netguard.ParseOrigin(destination); err != nil {
		return err
	}
	credential, _, err := s.credentials.Load(ctx, credentialID)
	if err != nil {
		return fmt.Errorf("load credential: %w", err)
	}
	if err := netguard.RequireSameOrigin(credential.SiteURL, destination); err != nil {
		return fmt.Errorf("credential %d cannot be attached to this site: %w", credentialID, err)
	}
	return nil
}
