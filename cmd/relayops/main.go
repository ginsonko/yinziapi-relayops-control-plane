package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/automation"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/bootstrap"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/collector"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/config"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/httpapi"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ledgersync"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/listing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorservice"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

const serverWriteTimeout = 5 * time.Minute

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := cfg.Validate(); err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	vault, err := secrets.Open(cfg.MasterKeyPath)
	if err != nil {
		logger.Error("open credential vault", "error", err)
		os.Exit(1)
	}
	credentialService := credentials.NewService(st, vault)
	resolvePolicy := netguard.ResolvePolicy{Attempts: cfg.DNSResolveAttempts, InitialDelay: cfg.DNSResolveInitialDelay,
		MaximumDelay: cfg.DNSResolveMaximumDelay, JitterPercent: cfg.DNSResolveJitterPercent}
	remoteWriteGate := remotewrite.New(cfg.RemoteWritesEnabled)
	httpClient := &http.Client{Transport: remoteWriteGate.Wrap(hardenedTransport(cfg.AllowLoopbackTargets, resolvePolicy)), Timeout: cfg.RequestTimeout}
	connectorFactory := connectors.NewFactory(httpClient, credentialService)
	connectorFactory.SetConnectorSpecLoader(st)
	connectorFactory.SetConnectorManifestLoader(st)
	collectorService := collector.NewService(st, connectorFactory)
	collectorService.SetTransientRetryDelay(cfg.CollectorTransientRetry)
	collectorService.SetJoinTimeout(cfg.CollectorJoinTimeout)
	targetFactory := targets.NewFactory(httpClient, credentialService)
	targetFactory.SetRemoteWriteGate(remoteWriteGate)
	visibilityVerifier, err := mirrorservice.NewVisibilityVerifier(targetFactory)
	if err != nil {
		logger.Error("construct complete-mirror visibility verifier", "error", err)
		os.Exit(1)
	}
	mirrorService, err := mirrorservice.New(st, targetFactory, visibilityVerifier, mirrorservice.Options{})
	if err != nil {
		logger.Error("construct complete-mirror service", "error", err)
		os.Exit(1)
	}
	automationService := automation.NewService(st, targetFactory)
	automationService.SetRemoteWriteGate(remoteWriteGate)
	listingService := listing.NewService(st, targetFactory, credentialService)
	listingService.SetRemoteWriteGate(remoteWriteGate)
	intelligenceService := intelligence.NewService(st)
	ledgerSyncService := ledgersync.NewService(st, credentialService, httpClient)
	ledgerSyncService.SetTargetFactory(targetFactory)
	credentialRefresher := credentials.NewRefresher(st, credentialService, httpClient, cfg.CredentialRefreshInterval).
		WithRemoteWriteGate(remoteWriteGate).
		WithRefreshAhead(cfg.CredentialRefreshAhead).
		WithLoginPolicy(credentials.LoginPolicy{MinimumInterval: cfg.CredentialLoginMinInterval,
			InitialBackoff: cfg.CredentialLoginInitialBackoff, MaximumBackoff: cfg.CredentialLoginMaximumBackoff,
			MaximumFailures: cfg.CredentialLoginMaximumFailures, HumanActionCooldown: cfg.CredentialLoginHumanCooldown}).
		WithCandidateValidator(func(ctx context.Context, supplier domain.Supplier, credential domain.Credential,
			payload domain.CredentialPayload) error {
			_, err := collectorService.ValidateCredential(ctx, supplier, credential.Kind, payload)
			return err
		})
	collectorService.SetAuthExpiredHook(credentialRefresher.SignalAuthExpired)
	automationService.SetSupplierRefresher(func(ctx context.Context, supplierID int64) error {
		_, err := collectorService.CollectOrJoin(ctx, supplierID)
		return err
	})
	mirrorService.SetSupplierRefresher(func(ctx context.Context, supplierID int64) error {
		_, err := collectorService.CollectOrJoin(ctx, supplierID)
		return err
	})
	collectorService.SetAfterCollectWithError(func(ctx context.Context, supplierID int64) error {
		return reconcileAfterCollect(ctx, supplierID, mirrorService.ReconcileSupplier,
			automationService.ReconcileSupplierWithError)
	})

	if _, err := bootstrap.Load(context.Background(), st, cfg.BootstrapPath); err != nil {
		logger.Error("load bootstrap", "error", err)
		os.Exit(1)
	}
	automationControl, err := st.ResolveAutomationControl(context.Background(), cfg.AutomationEnabled, cfg.BackgroundWorkersEnabled)
	if err != nil {
		logger.Error("load automation control", "error", err)
		os.Exit(1)
	}
	effectiveAutomationEnabled := automationControl.Enabled && remoteWriteGate.Enabled()
	automationService.SetAutomaticWritesEnabled(effectiveAutomationEnabled)
	mirrorService.SetAutomaticWritesEnabled(effectiveAutomationEnabled)
	if !effectiveAutomationEnabled {
		logger.Warn("automatic pricing writes are disabled; post-collection recalculation remains active")
	}
	if !remoteWriteGate.Enabled() {
		logger.Warn("remote writes are disabled for this local acceptance candidate")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	scheduler := collector.NewScheduler(st, collectorService, cfg.SchedulerTick, cfg.MaxCollectorWorkers)
	credentialService.SetCredentialUpdatedHook(scheduler.Wake)
	if cfg.BackgroundWorkersEnabled {
		go scheduler.Run(ctx)
		go credentialRefresher.Run(ctx)
		go runIntelligenceReconciler(ctx, logger, intelligenceService)
		go runRetentionWorker(ctx, logger, intelligenceService, st, productionRetentionWorkerOptions())
		go ledgerSyncService.Run(ctx)
		go func() {
			recoveryCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			if err := listingService.RecoverIncomplete(recoveryCtx); err != nil {
				logger.Warn("recover incomplete listing changes", "error", err)
			}
		}()
	} else {
		logger.Warn("background workers are disabled; only explicit API actions will run")
	}

	handler := httpapi.New(httpapi.Dependencies{Store: st, Credentials: credentialService, Refresher: credentialRefresher, Collector: collectorService,
		Automation: automationService, Mirror: mirrorService, Listing: listingService, LedgerSync: ledgerSyncService, Targets: targetFactory,
		HTTPClient: httpClient, StaticDir: cfg.StaticDir, AdminToken: cfg.AdminToken,
		RunwayHours: cfg.RunwayAlertHours, CredentialRefreshAhead: cfg.CredentialRefreshAhead,
		AutomationEnabled:        effectiveAutomationEnabled,
		BackgroundWorkersEnabled: cfg.BackgroundWorkersEnabled,
		RemoteWriteGate:          remoteWriteGate,
		PairingMaximumAttempts:   cfg.PairingMaximumAttempts,
		PairingTransientCooldown: cfg.PairingTransientCooldown})
	server := &http.Server{Addr: cfg.ListenAddress, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: serverWriteTimeout, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: 1 << 20}

	go func() {
		logger.Info("RelayOps started", "address", cfg.ListenAddress, "data_dir", cfg.DataDir)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown", "error", err)
	}
}

func reconcileAfterCollect(ctx context.Context, supplierID int64,
	nativeMirror func(context.Context, int64) error,
	legacyPricing func(context.Context, int64) error,
) error {
	mirrorErr := nativeMirror(ctx, supplierID)
	legacyErr := legacyPricing(ctx, supplierID)
	return errors.Join(mirrorErr, legacyErr)
}

func runIntelligenceReconciler(ctx context.Context, logger *slog.Logger, service *intelligence.Service) {
	const interval = 5 * time.Minute
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_, err := service.ReconcileDueOutcomes(ctx, 5000)
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("reconcile intelligence outcomes", "error", err)
			}
			timer.Reset(interval)
		}
	}
}

func hardenedTransport(allowLoopback bool, resolvePolicy netguard.ResolvePolicy) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tlsConfig := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	dialContext := netguard.PublicDialContextWithPolicy(dialer, resolvePolicy)
	dialTLSContext := netguard.PublicDialTLSContextWithPolicy(dialer, tlsConfig, resolvePolicy)
	if allowLoopback {
		dialContext = netguard.PublicOrLoopbackDialContextWithPolicy(dialer, resolvePolicy)
		dialTLSContext = netguard.PublicOrLoopbackDialTLSContextWithPolicy(dialer, tlsConfig, resolvePolicy)
	}
	return &http.Transport{
		DialContext:           dialContext,
		DialTLSContext:        dialTLSContext,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
}
