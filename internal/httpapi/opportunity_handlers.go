package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/opportunity"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

const opportunityConfigKey = "opportunity.config.v1"

type opportunitySummary struct {
	Recommended      int `json:"recommended"`
	Watch            int `json:"watch"`
	NoAdvantage      int `json:"no_advantage"`
	InsufficientData int `json:"insufficient_data"`
}

type opportunityTargetEvidence struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at,omitempty"`
	Stale      bool   `json:"stale"`
}

type opportunityResponse struct {
	Items          []opportunity.Result       `json:"items"`
	Summary        opportunitySummary         `json:"summary"`
	Target         *opportunityTargetEvidence `json:"target,omitempty"`
	Warnings       []string                   `json:"warnings"`
	HasObservedUse bool                       `json:"has_observed_usage"`
	GeneratedAt    string                     `json:"generated_at"`
}

func (s *Server) handleOpportunities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	config := s.opportunityConfig(ctx)
	discoveries, err := s.store.ListOpportunityDiscoveries(ctx, false, 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	suppliers, err := s.store.ListSuppliers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	supplierByID := make(map[int64]domain.Supplier, len(suppliers))
	catalogs := make([]opportunity.CatalogEvidence, 0, len(suppliers))
	for _, supplier := range suppliers {
		supplierByID[supplier.ID] = supplier
		if snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, supplier.ID); snapshotErr == nil {
			catalogs = append(catalogs, opportunity.CatalogEvidence{Supplier: supplier, Snapshot: snapshot})
		}
	}

	response := opportunityResponse{Items: []opportunity.Result{}, Warnings: []string{}, GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	targetCatalog, targetEvidence, targetWarning := s.loadOpportunityTargetCatalog(ctx, r, config)
	if targetEvidence != nil {
		response.Target = targetEvidence
	}
	if targetWarning != "" {
		response.Warnings = append(response.Warnings, targetWarning)
	}
	usageTargetID := int64(0)
	if response.Target != nil {
		usageTargetID = response.Target.ID
	}
	usageRows, usageErr := s.store.ListOpportunityUsage(ctx, usageTargetID, 45*24*time.Hour)
	if usageErr != nil {
		response.Warnings = append(response.Warnings, "读取本站模型用量失败，已改用明确标记的低置信先验")
	}
	forceRefresh := strings.EqualFold(r.URL.Query().Get("refresh"), "true")
	usageFresh := opportunityUsageFresh(usageRows, time.Duration(config.UsageTTLSeconds)*time.Second)
	if response.Target != nil && s.httpClient != nil && s.credentials != nil && (forceRefresh || !usageFresh) {
		if target, targetErr := s.store.GetTarget(ctx, response.Target.ID); targetErr == nil {
			observed, readErr := opportunity.ReadNewAPIUsage(ctx, s.httpClient, s.credentials, target, time.Now().UTC())
			if readErr == nil {
				observed = completeOpportunityZeroUsage(observed, targetCatalog, time.Now().UTC())
				rows := make([]store.OpportunityUsage, 0, len(observed))
				for _, item := range observed {
					rows = append(rows, store.OpportunityUsage{TargetSiteID: target.ID, ModelID: item.ModelID, WindowDays: item.WindowDays,
						DemandWeight: item.DemandWeight, DemandUnit: item.DemandUnit, Source: "newapi_data_flow", Confidence: item.Confidence,
						ObservedAt: item.ObservedAt, EvidenceRevision: "newapi-data-flow-v1"})
				}
				if saveErr := s.store.UpsertOpportunityUsage(ctx, rows); saveErr == nil {
					usageRows, usageErr = s.store.ListOpportunityUsage(ctx, target.ID, 45*24*time.Hour)
				} else {
					response.Warnings = append(response.Warnings, "本站模型用量已读取，但本次证据缓存保存失败")
				}
			} else if len(usageRows) > 0 {
				response.Warnings = append(response.Warnings, "本站模型用量实时刷新失败，已继续使用最近可信用量")
			} else {
				response.Warnings = append(response.Warnings, "目标站暂未提供可用的模型级用量接口，当前使用明确标记的低置信需求先验")
			}
		}
	}
	usage := make([]opportunity.UsageEvidence, 0, len(usageRows))
	for _, item := range usageRows {
		usage = append(usage, opportunity.UsageEvidence{ModelID: item.ModelID, WindowDays: item.WindowDays,
			DemandWeight: item.DemandWeight, DemandUnit: item.DemandUnit, Confidence: item.Confidence, ObservedAt: item.ObservedAt})
	}
	response.HasObservedUse = len(usage) > 0

	discoveryEvidence := make([]opportunity.DiscoveryEvidence, 0, len(discoveries))
	for _, item := range discoveries {
		supplier := supplierByID[item.SupplierID]
		fingerprint := opportunity.ContentFingerprint(supplier.BaseURL, item.Offer)
		offer := item.Offer
		if enriched, enrichErr := ratios.EnrichSnapshot(domain.CatalogSnapshot{Groups: []domain.GroupOffer{offer}}, supplier.RechargeValueFactor); enrichErr == nil {
			offer = enriched.Groups[0]
		}
		discoveryEvidence = append(discoveryEvidence, opportunity.DiscoveryEvidence{ID: discoveryID(item.DiscoveryItem),
			SupplierID: item.SupplierID, Supplier: item.SupplierName, SupplierBaseURL: supplier.BaseURL,
			SupplierEnabled: supplier.Enabled, SupplierStatus: supplier.LastStatus, SupplierErrorClass: supplier.LastErrorClass,
			Offer: offer, Fingerprint: fingerprint, ObservedAt: item.LastSeenAt})
	}
	currentRoutes := map[string]struct{}{}
	if response.Target != nil {
		if policies, policyErr := s.store.ListPolicies(ctx); policyErr == nil {
			for _, policy := range policies {
				if policy.Enabled && policy.TargetSiteID == response.Target.ID && strings.TrimSpace(policy.SourceGroup) != "" {
					currentRoutes[opportunity.RouteKey(policy.SupplierID, policy.SourceGroup)] = struct{}{}
				}
			}
		} else {
			response.Warnings = append(response.Warnings, "当前采购路线映射读取失败，节省金额将保持数据不足")
		}
	}
	candidates := opportunity.BuildCandidates(opportunity.BuildInput{Discoveries: discoveryEvidence, Catalogs: catalogs,
		Target: targetCatalog, Usage: usage, CurrentRoutes: currentRoutes, Config: config})
	response.Items = opportunity.EvaluateAll(candidates, config)
	for _, item := range response.Items {
		switch item.Classification {
		case opportunity.ClassificationRecommended:
			response.Summary.Recommended++
		case opportunity.ClassificationWatch:
			response.Summary.Watch++
		case opportunity.ClassificationNoAdvantage:
			response.Summary.NoAdvantage++
		default:
			response.Summary.InsufficientData++
		}
	}
	persist := make([]store.OpportunityEvaluationRecord, 0, len(response.Items))
	for _, item := range response.Items {
		persist = append(persist, store.OpportunityEvaluationRecord{CandidateID: item.ID, InputHash: item.InputHash,
			Classification: item.Classification, Payload: item})
	}
	if err := s.store.SaveOpportunityEvaluations(ctx, persist); err != nil {
		response.Warnings = append(response.Warnings, "收益建议已计算，但本次审计快照保存失败")
	}
	writeJSON(w, http.StatusOK, response)
}

func opportunityUsageFresh(items []store.OpportunityUsage, ttl time.Duration) bool {
	if len(items) == 0 || ttl <= 0 {
		return false
	}
	latest := items[0].ObservedAt
	for _, item := range items[1:] {
		if item.ObservedAt.After(latest) {
			latest = item.ObservedAt
		}
	}
	return time.Since(latest) <= ttl
}

func completeOpportunityZeroUsage(items []opportunity.ObservedUsage, target domain.CatalogSnapshot, observedAt time.Time) []opportunity.ObservedUsage {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		seen[strings.ToLower(strings.TrimSpace(item.ModelID))+":"+strconv.Itoa(item.WindowDays)] = struct{}{}
	}
	for _, model := range target.Models {
		modelID := strings.TrimSpace(model.ExternalID)
		if modelID == "" {
			modelID = strings.TrimSpace(model.Name)
		}
		if modelID == "" {
			continue
		}
		for _, days := range []int{7, 30} {
			key := strings.ToLower(modelID) + ":" + strconv.Itoa(days)
			if _, exists := seen[key]; exists {
				continue
			}
			items = append(items, opportunity.ObservedUsage{ModelID: modelID, WindowDays: days, DemandWeight: "0", DemandUnit: "unknown",
				Confidence: "0.95", ObservedAt: observedAt})
		}
	}
	return items
}

func (s *Server) handleOpportunityConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.opportunityConfig(r.Context()))
		return
	}
	var config opportunity.Config
	if err := readJSON(r, &config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	config = opportunity.NormalizeConfig(config)
	if err := opportunity.ValidateConfig(config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetSetting(r.Context(), opportunityConfigKey, config); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) handleOpportunityUsage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Observations []store.OpportunityUsage `json:"observations"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Observations) == 0 || len(input.Observations) > 5000 {
		writeError(w, http.StatusBadRequest, "observations must contain between 1 and 5000 rows")
		return
	}
	if err := s.store.UpsertOpportunityUsage(r.Context(), input.Observations); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(input.Observations)})
}

func (s *Server) opportunityConfig(ctx context.Context) opportunity.Config {
	config := opportunity.DefaultConfig()
	var stored opportunity.Config
	if err := s.store.GetSetting(ctx, opportunityConfigKey, &stored); err == nil {
		config = opportunity.NormalizeConfig(stored)
	}
	return config
}

func (s *Server) loadOpportunityTargetCatalog(ctx context.Context, r *http.Request, config opportunity.Config) (domain.CatalogSnapshot, *opportunityTargetEvidence, string) {
	targetList, err := s.store.ListTargets(ctx)
	if err != nil {
		return domain.CatalogSnapshot{}, nil, "目标站点目录不可读取，收益建议暂按数据不足展示"
	}
	var selected domain.TargetSite
	requestedID := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("target_id")); raw != "" {
		requestedID, _ = strconv.ParseInt(raw, 10, 64)
	}
	for _, target := range targetList {
		if requestedID > 0 && target.ID == requestedID {
			selected = target
			break
		}
		if requestedID == 0 && selected.ID == 0 && target.Enabled {
			selected = target
		}
	}
	if selected.ID == 0 {
		return domain.CatalogSnapshot{}, nil, "尚未配置可读的目标 NewAPI，候选会保留为数据不足"
	}
	cache, cacheErr := s.store.LatestOpportunityTargetCatalog(ctx, selected.ID)
	ttl := time.Duration(config.TargetCatalogTTLSeconds) * time.Second
	cacheFresh := cacheErr == nil && time.Since(cache.ObservedAt) <= ttl
	force := strings.EqualFold(r.URL.Query().Get("refresh"), "true")
	if cacheFresh && !force {
		return cache.Snapshot, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "cache",
			ObservedAt: cache.ObservedAt.Format(time.RFC3339)}, ""
	}
	if s.targets != nil {
		base, adapterErr := s.targets.ForTarget(selected)
		if adapterErr == nil {
			var channelAdapter targets.ChannelAdapter
			channelAdapter, adapterErr = targets.AsChannelAdapter(base)
			if adapterErr == nil {
				var snapshot domain.CatalogSnapshot
				snapshot, adapterErr = channelAdapter.PricingCatalog(ctx, selected)
				if adapterErr == nil {
					if saveErr := s.store.SaveOpportunityTargetCatalog(ctx, selected.ID, snapshot); saveErr != nil {
						return snapshot, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "live",
							ObservedAt: snapshot.Evidence.ObservedAt.Format(time.RFC3339)}, "目标目录已读取，但本次缓存保存失败"
					}
					return snapshot, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "live",
						ObservedAt: snapshot.Evidence.ObservedAt.Format(time.RFC3339)}, ""
				}
			}
		}
		if cacheErr == nil {
			return cache.Snapshot, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "stale_cache",
				ObservedAt: cache.ObservedAt.Format(time.RFC3339), Stale: true}, "目标目录实时刷新失败，已保留并使用最近可信缓存"
		}
		if adapterErr != nil {
			return domain.CatalogSnapshot{}, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "unavailable", Stale: true},
				"目标目录尚未取得，收益建议暂按数据不足展示"
		}
	}
	if cacheErr == nil {
		return cache.Snapshot, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "stale_cache",
			ObservedAt: cache.ObservedAt.Format(time.RFC3339), Stale: true}, "目标目录缓存已过期；当前运行未配置读取适配器"
	}
	if errors.Is(cacheErr, sql.ErrNoRows) {
		return domain.CatalogSnapshot{}, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "missing", Stale: true},
			"目标目录尚未采样，点击刷新后再查看收益建议"
	}
	return domain.CatalogSnapshot{}, &opportunityTargetEvidence{ID: selected.ID, Name: selected.Name, Source: "unavailable", Stale: true}, fmt.Sprintf("读取目标目录缓存失败：%v", cacheErr)
}
