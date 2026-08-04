import { Component, useEffect, useMemo, useState, type CSSProperties, type ErrorInfo, type ReactNode } from 'react';
import {
	Activity, ArrowRight, BrainCircuit, Check, CircleDollarSign, Clock3, Database,
	ChevronDown, ChevronUp, Gauge, Info, RefreshCw, Save, Scale, Search, Settings2, ShieldCheck, TriangleAlert,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type {
	IntelligenceConfig, IntelligenceDashboard, IntelligenceEvidenceLevel,
	IntelligenceHorizonForecast, IntelligenceRecommendation, LedgerExclusionPolicy,
	LedgerDimensionSummary, LedgerFinancialSummary, LedgerPricingContractSummary, OperatingSnapshot,
	ProfitDataQuality, UnifiedMoney, UnifiedRatio,
} from '../api/types';
import type { PageId } from '../components/Shell';
import { Button, EmptyState, ErrorState, formatRunway, Modal, PageHeader, Section, StatusBadge } from '../components/ui';

const evidenceLabels: Record<IntelligenceEvidenceLevel, string> = {
	exact: '精确', observed: '观测', estimated: '估算', prior: '先验', missing: '缺失',
};

const lifecycleLabels: Record<string, string> = {
	awaiting_decision: '等待决策', tracking: '跟踪结果', rejected: '已拒绝', no_intervention: '无需变更',
	rolled_back: '已回滚', superseded: '证据已变化', failed: '执行未完成',
};

const outcomeLabels: Record<string, string> = {
	awaiting_decision: '等待决策', not_due: '观察中', awaiting_ledger: '等待账本', observed: '已核对', not_applicable: '不纳入校准',
	coverage_unknown: '覆盖分母未知', insufficient_coverage: '覆盖率不足', no_exposure: '零暴露', censored: '窗口已截断',
};

const exclusionLabels: Record<string, string> = {
	administrator: '管理员', test_user: '测试账号', internal_token: '内部令牌',
	user_segment: '指定用户段', user_id: '指定用户', token_id: '指定令牌',
};

const streamEvidenceLabels: Record<string, string> = {
	exact: '完整现金字段', observed: '观测字段', observed_or_better: '观测或更高证据',
	estimated: '估算字段', estimated_or_better: '估算或更高证据', prior_or_better: '先验或更高证据',
	missing: '字段缺失', mixed: '混合证据',
};

function numeric(raw?: string) {
	if (raw === undefined || raw.trim() === '') return null;
	const value = Number(raw);
	return Number.isFinite(value) ? value : null;
}

function unifiedMoney(metric: UnifiedMoney) {
	const value = numeric(metric.amount);
	if (value === null) return '未知';
	const symbol = metric.currency === 'CNY' ? '¥/$' : metric.currency === 'USD' ? '$' : metric.currency;
	const output = `${symbol} ${value.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
	return metric.available ? output : `已知 ${output}`;
}

function unifiedRatio(metric: UnifiedRatio) {
	const value = numeric(metric.value);
	return metric.available && value !== null ? `${(value * 100).toFixed(2)}%` : '未知';
}

function estimatedRatio(metric: UnifiedRatio) {
	const value = numeric(metric.value);
	if (value === null) return '未知';
	const output = `${(value * 100).toFixed(2)}%`;
	return metric.available ? output : `已知估算 ${output}`;
}

function ratioEvidence(metric: UnifiedRatio) {
	if (!metric.available) return metric.note ?? '暂无可验证的匹配收入和采购成本';
	const coverage = metric.coverage ? `，匹配覆盖 ${(Number(metric.coverage) * 100).toFixed(1)}%` : '';
	const basis = metric.numerator && metric.denominator ? `分子 ${metric.numerator} / 分母 ${metric.denominator}` : '';
	return `${metric.evidence === 'ledger_matched_partial' ? '部分匹配' : '已匹配'}${coverage}${basis ? `，${basis}` : ''}。${metric.note ?? ''}`;
}

function compact(raw?: string, digits = 4) {
	const value = numeric(raw);
	if (value === null) return '未知';
	if (Math.abs(value) >= 1000) return value.toLocaleString('zh-CN', { maximumFractionDigits: 1 });
	return value.toLocaleString('zh-CN', { maximumFractionDigits: digits });
}

function percent(raw?: string) {
	const value = numeric(raw);
	return value === null ? '未知' : `${(value * 100).toFixed(1)}%`;
}

function money(raw: string | undefined, currency: string) {
	const value = numeric(raw);
	if (value === null) return '未知';
	return `${value.toLocaleString('zh-CN', { maximumFractionDigits: 4 })} ${currency}`;
}

function coverageTone(status: string) {
	if (status === 'verified' || status === 'complete') return 'healthy' as const;
	if (status === 'unknown' || status === 'missing') return 'error' as const;
	return 'warning' as const;
}

function splitList(value: string) {
	return [...new Set(value.split(/[\s,，;；]+/).map((item) => item.trim()).filter(Boolean))];
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === 'object' && value !== null;
}

function validForecast(value: unknown): value is IntelligenceHorizonForecast {
	if (!isRecord(value) || !isRecord(value.approve) || !isRecord(value.reject) || !isRecord(value.delta_contribution)) return false;
	return isRecord(value.approve.contribution) && isRecord(value.reject.contribution);
}

function normalizeIntelligenceDashboard(raw: IntelligenceDashboard): IntelligenceDashboard {
	const recommendations = Array.isArray(raw.recommendations) ? raw.recommendations.map((item) => {
		const rawForecasts = Array.isArray(item.forecasts) ? item.forecasts : [];
		const forecasts = rawForecasts.filter(validForecast);
		const warnings = Array.isArray(item.warnings) ? [...item.warnings] : [];
		if (forecasts.length !== rawForecasts.length) warnings.push(`已跳过 ${rawForecasts.length - forecasts.length} 条结构不完整的历史预测记录`);
		return {
			...item,
			forecasts,
			evidence: Array.isArray(item.evidence) ? item.evidence : [],
			warnings,
			model_version: item.model_version || 'unknown',
			input_hash: item.input_hash || 'unavailable',
		};
	}) : [];
	const byCurrency = Array.isArray(raw.financials?.by_currency) ? raw.financials.by_currency : [];
	const missingRevenue = raw.financials?.unknown_usage_revenue_events ?? byCurrency.reduce((sum, item) => sum + (item.unknown_usage_revenue_events || 0), 0);
	const missingCost = raw.financials?.unknown_usage_cost_events ?? byCurrency.reduce((sum, item) => sum + (item.unknown_usage_cost_events || 0), 0);
	const missingCash = raw.financials?.unknown_cash_flow_amount_events ?? byCurrency.reduce((sum, item) => sum + (item.unknown_cash_flow_amount_events || 0), 0);
	const displayCurrency = raw.config?.display_currency || 'CNY';
	const missingMoney = (note: string): UnifiedMoney => ({ currency: displayCurrency, available: false, known_items: 0, unknown_items: 0, evidence: 'missing', note });
	const operations: OperatingSnapshot = raw.operations ? {
		...raw.operations,
		currency_rates: raw.operations.currency_rates ?? { CNY: '1', USD: '1' },
		today_commission_cost: raw.operations.today_commission_cost ?? missingMoney('当前服务版本尚未返回消费佣金成本'),
		today_net_profit: raw.operations.today_net_profit ?? missingMoney('当前服务版本尚未返回扣佣后净利润'),
		today_net_margin: raw.operations.today_net_margin ?? { available: false, evidence: 'missing', matched_items: 0, total_items: 0, coverage: '0', note: '当前服务版本尚未返回扣佣后净利润率' },
		operating_gap_revenue: raw.operations.operating_gap_revenue ?? missingMoney('尚未生成包含管理员的全站扣费口径'),
		operating_gap_includes_administrators: raw.operations.operating_gap_includes_administrators ?? false,
		operating_gap_estimate: raw.operations.operating_gap_estimate ?? missingMoney('尚未生成运营差额估算'),
		operating_gap_margin: raw.operations.operating_gap_margin ?? { available: false, evidence: 'missing', matched_items: 0, total_items: 0, coverage: '0', note: '尚未生成运营差额估算' },
		profit_data_quality: raw.operations.profit_data_quality ? {
			...raw.operations.profit_data_quality,
			cost_resolution_scanned_events: raw.operations.profit_data_quality.cost_resolution_scanned_events ?? 0,
			cost_resolution_resolved_events: raw.operations.profit_data_quality.cost_resolution_resolved_events ?? 0,
			cost_resolution_unresolved_events: raw.operations.profit_data_quality.cost_resolution_unresolved_events ?? 0,
			cost_resolution_truncated: raw.operations.profit_data_quality.cost_resolution_truncated ?? false,
			cost_resolution_reasons: Array.isArray(raw.operations.profit_data_quality.cost_resolution_reasons) ? raw.operations.profit_data_quality.cost_resolution_reasons : [],
		} : {
			window_start: raw.operations.day_started_at, window_end: raw.operations.generated_at,
			included_usage_events: 0, revenue_known_events: 0, revenue_unknown_events: 0, cost_known_events: 0,
			cost_unknown_events: 0, unknown_currency_events: 0, revenue_coverage: '0', cost_coverage: '0',
			ledger_coverage_status: 'unknown', strict_profit_available: false, operating_estimate_ready: false,
			required_ledger_fields: [], blockers: ['当前服务版本尚未返回利润数据质量说明'],
			next_action: '重新加载控制台；若仍缺失，请确认服务端版本。', operating_estimate_basis: '',
			cost_resolution_scanned_events: 0, cost_resolution_resolved_events: 0, cost_resolution_unresolved_events: 0,
			cost_resolution_truncated: false, cost_resolution_reasons: [],
		},
		supplier_positions: Array.isArray(raw.operations.supplier_positions) ? raw.operations.supplier_positions : [],
		groups: Array.isArray(raw.operations.groups) ? raw.operations.groups : [],
		warnings: Array.isArray(raw.operations.warnings) ? raw.operations.warnings : [],
	} : {
		display_currency: displayCurrency, currency_rates: { CNY: '1', USD: '1' }, business_time_zone: 'Asia/Hong_Kong',
		day_started_at: raw.generated_at, admin_excluded: true, user_balance: missingMoney('尚未生成统一现金快照'),
		today_user_spend: missingMoney('尚未生成统一现金快照'), upstream_balance: missingMoney('尚未生成统一现金快照'),
		upstream_daily_spend: missingMoney('尚未生成统一现金快照'), today_revenue: missingMoney('尚未生成统一现金快照'),
		today_cost: missingMoney('尚未生成统一现金快照'), today_gross_profit: missingMoney('尚未生成统一现金快照'),
		today_gross_margin: { available: false, evidence: 'missing', matched_items: 0, total_items: 0, coverage: '0', note: '尚未生成统一现金快照' },
		today_commission_cost: missingMoney('尚未生成消费佣金成本'), today_net_profit: missingMoney('尚未生成扣佣后净利润'),
		today_net_margin: { available: false, evidence: 'missing', matched_items: 0, total_items: 0, coverage: '0', note: '尚未生成扣佣后净利润率' },
		operating_gap_revenue: missingMoney('尚未生成包含管理员的全站扣费口径'), operating_gap_includes_administrators: false,
		operating_gap_estimate: missingMoney('尚未生成运营差额估算'),
		operating_gap_margin: { available: false, evidence: 'missing', matched_items: 0, total_items: 0, coverage: '0', note: '尚未生成运营差额估算' },
		profit_data_quality: { window_start: raw.generated_at, window_end: raw.generated_at, included_usage_events: 0,
			revenue_known_events: 0, revenue_unknown_events: 0, cost_known_events: 0, cost_unknown_events: 0,
			unknown_currency_events: 0, revenue_coverage: '0', cost_coverage: '0', ledger_coverage_status: 'unknown',
			strict_profit_available: false, operating_estimate_ready: false, required_ledger_fields: [],
			blockers: ['尚未生成统一现金快照'], next_action: '先完成本站经营账本同步。', operating_estimate_basis: '',
			cost_resolution_scanned_events: 0, cost_resolution_resolved_events: 0, cost_resolution_unresolved_events: 0,
			cost_resolution_truncated: false, cost_resolution_reasons: [] },
		supplier_positions: [], groups: [], warnings: [], generated_at: raw.generated_at,
	};
	return {
		...raw,
		operations,
		config: { ...raw.config, display_currency: displayCurrency, currency_rates: raw.config?.currency_rates ?? { CNY: '1', USD: '1' }, business_time_zone: raw.config?.business_time_zone || 'Asia/Hong_Kong' },
		recommendations,
		warnings: Array.isArray(raw.warnings) ? raw.warnings : [],
		evidence_legend: Array.isArray(raw.evidence_legend) ? raw.evidence_legend : [],
		financials: {
			...raw.financials,
			included_usage_event_count: raw.financials?.included_usage_event_count ?? byCurrency.reduce((sum, item) => sum + (item.usage_event_count || 0), 0),
			unknown_usage_currency_events: raw.financials?.unknown_usage_currency_events ?? 0,
			by_currency: byCurrency,
			excluded_by_currency: Array.isArray(raw.financials?.excluded_by_currency) ? raw.financials.excluded_by_currency : [],
			user_segments: Array.isArray(raw.financials?.user_segments) ? raw.financials.user_segments : [],
			groups: Array.isArray(raw.financials?.groups) ? raw.financials.groups : [],
			excluded_by_reason: raw.financials?.excluded_by_reason ?? {},
			warnings: Array.isArray(raw.financials?.warnings) ? raw.financials.warnings : [],
			unknown_usage_revenue_events: missingRevenue,
			unknown_usage_cost_events: missingCost,
			unknown_cash_flow_amount_events: missingCash,
		},
	};
}

class IntelligencePageBoundary extends Component<{ children: ReactNode }, { error: string }> {
	state = { error: '' };
	static getDerivedStateFromError(error: unknown) {
		return { error: error instanceof Error ? error.message : '页面数据存在不兼容字段' };
	}
	componentDidCatch(_error: Error, _info: ErrorInfo) {}
	render() {
		if (this.state.error) return <ErrorState message={`经营智能的单页渲染已被隔离：${this.state.error}`} onRetry={() => window.location.reload()} />;
		return this.props.children;
	}
}

function MoneyEvidence({ raw, currency, unknownCount }: { raw?: string; currency: string; unknownCount: number }) {
	const value = numeric(raw);
	if (unknownCount <= 0) return <>{money(raw, currency)}</>;
	const knownPart = value !== null && value !== 0;
	return <span className="financial-partial-value">
		<strong>{knownPart ? `已知部分 ${money(raw, currency)}` : '未知'}</strong>
		<small>{unknownCount.toLocaleString('zh-CN')} 项金额待补</small>
	</span>;
}

type SegmentView = 'included' | 'excluded';
const segmentPreviewLimit = 6;

function FinancialSegments({ financials }: { financials: LedgerFinancialSummary }) {
	const includedCount = financials.user_segments.filter((segment) => segment.included).length;
	const excludedCount = financials.user_segments.length - includedCount;
	const [view, setView] = useState<SegmentView>(includedCount > 0 ? 'included' : 'excluded');
	const [expanded, setExpanded] = useState(false);

	useEffect(() => {
		if (view === 'included' && includedCount === 0 && excludedCount > 0) setView('excluded');
		if (view === 'excluded' && excludedCount === 0 && includedCount > 0) setView('included');
	}, [excludedCount, includedCount, view]);

	const segments = useMemo(() => financials.user_segments
		.filter((segment) => view === 'included' ? segment.included : !segment.included)
		.sort((a, b) => b.requests - a.requests || b.event_count - a.event_count || a.user_segment.localeCompare(b.user_segment, 'zh-CN')), [financials.user_segments, view]);
	const visibleSegments = expanded ? segments : segments.slice(0, segmentPreviewLimit);
	const selectView = (next: SegmentView) => {
		setView(next);
		setExpanded(false);
	};

	if (financials.user_segments.length === 0) return <p className="financial-empty-line">同步用户与调用日志后显示分群覆盖率。</p>;

	return <>
		<header className="financial-segment-header">
			<div><strong>用户分群贡献</strong><span>主要分群按请求量排序；排除项单独保留，不污染经营结论</span></div>
			<div className="segmented financial-segment-tabs" role="tablist" aria-label="用户分群范围">
				<button type="button" role="tab" aria-selected={view === 'included'} className={view === 'included' ? 'active' : ''} onClick={() => selectView('included')} disabled={includedCount === 0}>主要分群 {includedCount}</button>
				<button type="button" role="tab" aria-selected={view === 'excluded'} className={view === 'excluded' ? 'active' : ''} onClick={() => selectView('excluded')} disabled={excludedCount === 0}>已排除 {excludedCount}</button>
			</div>
		</header>
		{segments.length === 0 ? <p className="financial-empty-line">当前分类没有分群记录。</p> : <>
			<div className="financial-segment-list">{visibleSegments.map((segment, index) => <div key={`${segment.user_segment}-${segment.currency}-${segment.included}-${index}`} className={!segment.included ? 'excluded' : ''}>
				<span><strong>{segment.user_segment || '未分类用户'}</strong><small>{segment.currency || '币种未知'} · {segment.requests.toLocaleString('zh-CN')} 次请求</small></span>
				<span><b>收入覆盖 {percent(segment.revenue_coverage)}</b><small>成本覆盖 {percent(segment.cost_coverage)}</small></span>
				<span><strong>{segment.contribution_available ? money(segment.contribution, segment.currency) : '贡献未知'}</strong><small>{segment.included ? '已纳入经营口径' : `已排除：${(segment.exclusion_reasons ?? []).map((reason) => exclusionLabels[reason] ?? reason).join('、')}`}</small></span>
			</div>)}</div>
			{segments.length > segmentPreviewLimit && <div className="financial-segment-footer"><span>{expanded ? `已显示全部 ${segments.length} 个分群` : `优先显示请求量最高的 ${segmentPreviewLimit} 个分群`}</span><button type="button" className="text-button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>{expanded ? <><ChevronUp size={14} />收起</> : <><ChevronDown size={14} />查看其余 {segments.length - segmentPreviewLimit} 个</>}</button></div>}
		</>}
	</>;
}

type FinancialBreakdownView = 'groups' | 'models' | 'contracts';

const ledgerFieldLabels: Record<string, string> = {
	cash_revenue: '用户扣费', procurement_cost: '采购成本', currency: '币种', group_name: '分组', model_id: '模型',
	pricing_scope: '计价范围', pricing_revision: '计价 revision', billing_unit: '计费单位', fixed_duration_seconds: '固定时长',
};

const billingUnitLabels: Record<string, string> = {
	per_request: '每次请求', per_second: '每秒', fixed_duration: '固定时长', token_vector: 'Token 向量', unknown: '单位未知',
};

function financialRowSort<T extends { known_revenue?: string; requests: number; event_count: number }>(left: T, right: T) {
	const revenueDifference = (numeric(right.known_revenue) ?? -1) - (numeric(left.known_revenue) ?? -1);
	return revenueDifference || right.requests - left.requests || right.event_count - left.event_count;
}

function missingFields(fields?: string[]) {
	return (fields ?? []).map((field) => ledgerFieldLabels[field] ?? field).join('、');
}

function shortRevision(revision?: string) {
	if (!revision) return '未记录';
	return revision.length > 16 ? `${revision.slice(0, 8)}…${revision.slice(-6)}` : revision;
}

function FinancialBreakdown({ financials }: { financials: LedgerFinancialSummary }) {
	const groups = financials.groups ?? [];
	const models = financials.models ?? [];
	const contracts = financials.pricing_contracts ?? [];
	const [view, setView] = useState<FinancialBreakdownView>(groups.length > 0 ? 'groups' : models.length > 0 ? 'models' : 'contracts');
	const [query, setQuery] = useState('');
	const normalizedQuery = query.trim().toLocaleLowerCase('zh-CN');
	const dimensions = useMemo(() => {
		const source = view === 'groups' ? groups : view === 'models' ? models : [];
		return [...source].filter((item) => !normalizedQuery || `${item.label} ${item.currency}`.toLocaleLowerCase('zh-CN').includes(normalizedQuery)).sort(financialRowSort);
	}, [groups, models, normalizedQuery, view]);
	const visibleContracts = useMemo(() => [...contracts].filter((item) => !normalizedQuery ||
		`${item.group_name} ${item.model_id} ${item.pricing_scope} ${item.pricing_revision ?? ''} ${item.billing_mode} ${item.billing_unit} ${item.currency}`
			.toLocaleLowerCase('zh-CN').includes(normalizedQuery)).sort(financialRowSort), [contracts, normalizedQuery]);
	const total = view === 'groups' ? groups.length : view === 'models' ? models.length : contracts.length;
	const visible = view === 'contracts' ? visibleContracts.length : dimensions.length;
	const description = view === 'groups' ? '按本站目标分组汇总；同币种内收入、成本与利润保持同一口径。' : view === 'models'
		? '按真实模型名汇总跨分组贡献；不同币种仍分行，不会静默合并。'
		: '按目标分组、模型、计价范围、revision、单位和币种隔离；可定位每版热更新合同的真实结果。';
	const selectView = (next: FinancialBreakdownView) => {
		setView(next);
		setQuery('');
	};
	return <section className="financial-breakdown">
		<header>
			<div><strong>真实收益拆解</strong><span>{description}</span></div>
			<div className="financial-breakdown-controls">
				<div className="segmented financial-breakdown-tabs" role="tablist" aria-label="经营收益维度">
					<button type="button" role="tab" aria-selected={view === 'groups'} className={view === 'groups' ? 'active' : ''} onClick={() => selectView('groups')}>分组 {groups.length}</button>
					<button type="button" role="tab" aria-selected={view === 'models'} className={view === 'models' ? 'active' : ''} onClick={() => selectView('models')}>模型 {models.length}</button>
					<button type="button" role="tab" aria-selected={view === 'contracts'} className={view === 'contracts' ? 'active' : ''} onClick={() => selectView('contracts')}>合同 revision {contracts.length}</button>
				</div>
				<label className="financial-breakdown-search"><Search size={14} /><input value={query} onChange={(event) => setQuery(event.target.value)} aria-label="筛选收益明细" placeholder="筛选分组、模型或 revision" /></label>
			</div>
		</header>
		<div className="financial-breakdown-count">显示 {visible.toLocaleString('zh-CN')} / {total.toLocaleString('zh-CN')} 项；利润只在该行所有请求的收入、采购成本和币种都完整时显示。</div>
		{visible === 0 ? <p className="financial-empty-line">{total === 0 ? '当前账本尚未提供这一维度；旧版后端会在这里安全回退为空。' : '没有匹配当前筛选条件的记录。'}</p> : view === 'contracts'
			? <PricingContractTable items={visibleContracts} />
			: <FinancialDimensionTable items={dimensions} kind={view === 'groups' ? '分组' : '模型'} />}
	</section>;
}

function FinancialDimensionTable({ items, kind }: { items: LedgerDimensionSummary[]; kind: '分组' | '模型' }) {
	return <div className="table-wrap financial-breakdown-table-wrap"><table className="financial-breakdown-table">
		<thead><tr><th>{kind}</th><th>调用</th><th>用户扣费</th><th>采购成本</th><th>真实毛利</th><th>毛利率</th><th>数据覆盖</th></tr></thead>
		<tbody>{items.map((item) => <tr key={`${item.key}-${item.currency}`}>
			<td><strong>{item.label}</strong><small>{item.currency}</small></td>
			<td><strong>{item.requests.toLocaleString('zh-CN')}</strong><small>{item.event_count.toLocaleString('zh-CN')} 条账本事件</small></td>
			<td><MoneyEvidence raw={item.known_revenue} currency={item.currency} unknownCount={item.event_count - item.revenue_known_events} /></td>
			<td><MoneyEvidence raw={item.known_cost} currency={item.currency} unknownCount={item.event_count - item.cost_known_events} /></td>
			<td className={item.profit_available && Number(item.gross_profit) < 0 ? 'is-negative' : ''}>{item.profit_available ? money(item.gross_profit, item.currency) : '未知'}</td>
			<td>{item.profit_available ? percent(item.gross_margin) : '未知'}</td>
			<td><strong>收入 {percent(item.revenue_coverage)} · 成本 {percent(item.cost_coverage)}</strong>{(item.requires_ledger_fields?.length ?? 0) > 0 && <small>待补：{missingFields(item.requires_ledger_fields)}</small>}</td>
		</tr>)}</tbody>
	</table></div>;
}

function PricingContractTable({ items }: { items: LedgerPricingContractSummary[] }) {
	return <div className="table-wrap financial-breakdown-table-wrap"><table className="financial-breakdown-table contract-table">
		<thead><tr><th>分组 / 模型</th><th>计价合同</th><th>调用</th><th>用户扣费</th><th>采购成本</th><th>真实毛利</th><th>覆盖</th></tr></thead>
		<tbody>{items.map((item) => <tr key={item.key}>
			<td><strong>{item.group_name}</strong><small>{item.model_id} · {item.currency}</small></td>
			<td><code title={item.pricing_revision || '未记录 revision'}>{shortRevision(item.pricing_revision)}</code><small>{item.pricing_scope} · {billingUnitLabels[item.billing_unit] ?? item.billing_unit}{item.fixed_duration_seconds ? ` ${item.fixed_duration_seconds} 秒` : ''} · {item.billing_mode}</small></td>
			<td><strong>{item.requests.toLocaleString('zh-CN')}</strong><small>{item.event_count.toLocaleString('zh-CN')} 条账本事件</small></td>
			<td><MoneyEvidence raw={item.known_revenue} currency={item.currency} unknownCount={item.event_count - item.revenue_known_events} /></td>
			<td><MoneyEvidence raw={item.known_cost} currency={item.currency} unknownCount={item.event_count - item.cost_known_events} /></td>
			<td className={item.profit_available && Number(item.gross_profit) < 0 ? 'is-negative' : ''}><strong>{item.profit_available ? money(item.gross_profit, item.currency) : '未知'}</strong><small>{item.profit_available ? `毛利率 ${percent(item.gross_margin)}` : '收入或成本仍有缺口'}</small></td>
			<td><strong>合同 {percent(item.contract_evidence_coverage)}</strong><small>收入 {percent(item.revenue_coverage)} · 成本 {percent(item.cost_coverage)}</small>{(item.requires_ledger_fields?.length ?? 0) > 0 && <small>待补：{missingFields(item.requires_ledger_fields)}</small>}</td>
		</tr>)}</tbody>
	</table></div>;
}

function FinancialOverview({ financials }: { financials: LedgerFinancialSummary }) {
	const excluded = Object.entries(financials.excluded_by_reason).sort((a, b) => b[1] - a[1]);
	const missingRevenue = financials.unknown_usage_revenue_events;
	const missingCost = financials.unknown_usage_cost_events;
	const missingCash = financials.unknown_cash_flow_amount_events;
	return <>
		<div className="financial-status-strip">
			<div><strong>{financials.raw_event_count.toLocaleString('zh-CN')} 条</strong><span>窗口内导入调用记录</span></div>
			<div><strong>{financials.included_event_count.toLocaleString('zh-CN')} 条</strong><span>纳入当前经营口径</span></div>
			<div><strong>{financials.excluded_event_count.toLocaleString('zh-CN')} 条</strong><span>按规则排除的记录</span></div>
			<div><strong>{missingCost.toLocaleString('zh-CN')} 条</strong><span>采购成本尚未关联</span></div>
			<StatusBadge status={coverageTone(financials.coverage.status)} label={financials.coverage.status === 'verified' ? '完整性已核验' : '完整性待核验'} />
		</div>
		<div className="ledger-scope-note"><Database size={18} /><div><strong>上方都是调用记录条数，不是人民币、美元或上游额度消耗</strong><span>统计窗口为 {new Date(financials.window_start).toLocaleDateString('zh-CN')} 至 {new Date(financials.window_end).toLocaleDateString('zh-CN')}。本站用量收入来自 NewAPI 日志 quota 换算；当前待补收入金额字段 {missingRevenue.toLocaleString('zh-CN')} 条、采购成本金额字段 {missingCost.toLocaleString('zh-CN')} 条、现金流金额字段 {missingCash.toLocaleString('zh-CN')} 笔。真实现金毛利必须等请求级上游采购成本关联后才会显示。</span></div></div>

		{financials.by_currency.length === 0 ? <EmptyState icon={<CircleDollarSign size={24} />} title="尚无可分币种展示的经营数据" description="本站同步器会每五分钟增量读取充值、调用和用户角色。币种或金额未知时保留为未知，不会显示成 0。" /> : <div className="table-wrap financial-table-wrap">
			<table className="financial-table">
				<thead><tr><th>币种</th><th>充值毛额</th><th>净现金流</th><th>用量收入</th><th>采购成本</th><th>用量毛利</th><th>毛利率</th><th>证据</th></tr></thead>
				<tbody>{financials.by_currency.map((item) => <tr key={item.currency}>
					<td><strong>{item.currency}</strong><small>{item.usage_event_count} 次调用 · {item.cash_flow_event_count} 笔现金事件</small></td>
					<td className="mono"><MoneyEvidence raw={item.recharge_gross} currency={item.currency} unknownCount={item.unknown_cash_flow_amount_events} /></td>
					<td className="mono">{item.cash_net_flow_available ? money(item.cash_net_flow, item.currency) : '未知'}</td>
					<td className="mono"><MoneyEvidence raw={item.usage_revenue} currency={item.currency} unknownCount={item.unknown_usage_revenue_events} /></td>
					<td className="mono"><MoneyEvidence raw={item.usage_procurement_cost} currency={item.currency} unknownCount={item.unknown_usage_cost_events} /></td>
					<td className="mono">{item.usage_profit_available ? money(item.usage_gross_profit, item.currency) : '未知'}</td>
					<td>{item.usage_profit_available ? percent(item.usage_gross_margin) : '未知'}</td>
					<td><StatusBadge status={item.unknown_usage_revenue_events + item.unknown_usage_cost_events + item.unknown_cash_flow_amount_events === 0 ? 'healthy' : 'warning'} label={streamEvidenceLabels[item.evidence_quality] ?? item.evidence_quality} /><small>{item.unknown_usage_revenue_events + item.unknown_usage_cost_events + item.unknown_cash_flow_amount_events} 项未知</small></td>
				</tr>)}</tbody>
			</table>
		</div>}

		<div className="financial-detail-grid">
			<div className="financial-detail-block"><FinancialSegments financials={financials} /></div>
			<div className="financial-detail-block"><header><strong>完整性与排除</strong><span>跨币种不静默合并</span></header>
				<div className="financial-coverage-copy"><StatusBadge status={coverageTone(financials.coverage.status)} label={`${financials.coverage.verified_cohorts}/${financials.coverage.cohorts} 个来源窗口已核验`} /><p>{financials.coverage.note}</p></div>
				<div className="financial-exclusions">{excluded.length === 0 ? <span>当前没有被排除的账本事件</span> : excluded.map(([reason, count]) => <span key={reason}><b>{exclusionLabels[reason] ?? reason}</b><strong>{count}</strong></span>)}</div>
				{financials.warnings.length > 0 && <div className="intelligence-warning-list">{financials.warnings.map((warning) => <span key={warning}><TriangleAlert size={15} />{warning}</span>)}</div>}
			</div>
		</div>
		<FinancialBreakdown financials={financials} />
	</>;
}

function costResolutionSampleText(sample: ProfitDataQuality['cost_resolution_reasons'][number]['samples'][number]) {
	return [sample.channel_name || (sample.channel_id ? `渠道 ${sample.channel_id}` : ''), sample.target_group ? `本站分组 ${sample.target_group}` : '',
		sample.supplier_name ? `上游 ${sample.supplier_name}` : '', sample.source_group ? `上游分组 ${sample.source_group}` : '',
		sample.model ? `模型 ${sample.model}` : ''].filter(Boolean).join(' / ');
}

function OperatingCashOverview({ operations }: { operations: OperatingSnapshot }) {
	const maximumGroupRequests = Math.max(1, ...operations.groups.map((item) => item.requests));
	const quality = operations.profit_data_quality;
	const costReasons = quality.cost_resolution_reasons ?? [];
	const grossEstimated = operations.today_gross_profit.evidence === 'estimated';
	const grossAmount = numeric(operations.today_gross_profit.amount);
	return <>
		<div className="operating-kpi-grid intelligence-operating-kpis">
			<article><span>本站用户总余额</span><strong>{unifiedMoney(operations.user_balance)}</strong><small>{operations.user_balance.note}</small></article>
			<article><span>今日用户总消耗</span><strong>{unifiedMoney(operations.today_user_spend)}</strong><small>{operations.admin_excluded ? '默认排除管理员、测试和内部调用' : '当前包含管理员调用'}</small></article>
			<article><span>上游总余额</span><strong>{unifiedMoney(operations.upstream_balance)}</strong><small>{operations.upstream_balance.unknown_items > 0 ? `${operations.upstream_balance.unknown_items} 个上游金额待补` : '启用上游已全部换算'}</small></article>
			<article><span>上游近 24h 消耗</span><strong>{unifiedMoney(operations.upstream_daily_spend)}</strong><small>{operations.upstream_daily_spend.note}</small></article>
			<article className={!operations.today_gross_profit.available ? 'unknown' : (grossAmount ?? 0) < 0 ? 'negative' : grossEstimated ? 'warning' : 'positive'}><span>{grossEstimated ? '今日现金毛利估算' : '今日现金毛利'}</span><strong>{unifiedMoney(operations.today_gross_profit)}</strong><small>{operations.today_gross_profit.note}</small></article>
			<article className={operations.today_gross_margin.available ? 'positive' : 'unknown'}><span>今日平均利润率</span><strong>{unifiedRatio(operations.today_gross_margin)}</strong><small>{ratioEvidence(operations.today_gross_margin)}</small></article>
			<article className={operations.today_commission_cost.available ? 'warning' : 'unknown'}><span>今日消费佣金净成本</span><strong>{unifiedMoney(operations.today_commission_cost)}</strong><small>{operations.today_commission_cost.note}</small></article>
			<article className={!operations.today_net_profit.available ? 'unknown' : (numeric(operations.today_net_profit.amount) ?? 0) < 0 ? 'negative' : 'positive'}><span>今日扣佣后净利润</span><strong>{unifiedMoney(operations.today_net_profit)}</strong><small>{operations.today_net_profit.note}</small></article>
			<article className={!operations.today_net_margin.available ? 'unknown' : (numeric(operations.today_net_margin.value) ?? 0) < 0 ? 'negative' : 'positive'}><span>今日扣佣后净利润率</span><strong>{unifiedRatio(operations.today_net_margin)}</strong><small>{ratioEvidence(operations.today_net_margin)}</small></article>
			<article><span>运营差额本站收入</span><strong>{unifiedMoney(operations.operating_gap_revenue)}</strong><small>{operations.operating_gap_includes_administrators ? '专用于全站对比，包含管理员扣费' : operations.operating_gap_revenue.note}</small></article>
			<article className={operations.operating_gap_estimate.available ? 'positive' : operations.operating_gap_estimate.amount ? 'warning' : 'unknown'}><span>运营消耗差额估算</span><strong>{unifiedMoney(operations.operating_gap_estimate)}</strong><small>{operations.operating_gap_estimate.note}</small></article>
			<article className={operations.operating_gap_margin.available ? 'positive' : operations.operating_gap_margin.value ? 'warning' : 'unknown'}><span>运营差额率估算</span><strong>{estimatedRatio(operations.operating_gap_margin)}</strong><small>{operations.operating_gap_margin.note}</small></article>
		</div>
		<section className="profit-quality-panel">
			<header><div><strong>为什么毛利是{quality.strict_profit_available ? '可核验值' : grossEstimated ? '估算值' : '未知'}</strong><span>严格账本与估算分开；缺失成本不会默认按 0</span></div><StatusBadge status={quality.strict_profit_available ? 'healthy' : 'warning'} label={quality.strict_profit_available ? '请求级成本完整' : grossEstimated ? `已估算 · 严格覆盖 ${(Number(quality.cost_coverage) * 100).toFixed(1)}%` : `成本覆盖 ${(Number(quality.cost_coverage) * 100).toFixed(1)}%`} /></header>
			<div className="profit-quality-grid">
				<div><span>纳入调用</span><strong>{quality.included_usage_events}</strong><small>业务日内且符合当前排除规则</small></div>
				<div><span>用户扣费覆盖</span><strong>{quality.revenue_known_events}/{quality.included_usage_events}</strong><small>{(Number(quality.revenue_coverage) * 100).toFixed(1)}%</small></div>
				<div><span>采购成本覆盖</span><strong>{quality.cost_known_events}/{quality.included_usage_events}</strong><small>{(Number(quality.cost_coverage) * 100).toFixed(1)}%</small></div>
				<div><span>账本窗口</span><strong>{quality.ledger_coverage_status === 'verified' ? '已核验' : '仅已导入'}</strong><small>{new Date(quality.window_start).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })} 至今</small></div>
			</div>
			{quality.blockers.length > 0 && <div className="profit-quality-blockers">{quality.blockers.map((item) => <span key={item}><TriangleAlert size={15} />{item}</span>)}</div>}
			{costReasons.length > 0 && <details className="profit-cost-diagnostics"><summary>成本回填诊断：仍有 {(quality.cost_resolution_unresolved_events ?? 0).toLocaleString('zh-CN')} 条待解析</summary><div>
				{costReasons.map((reason) => <div key={`${reason.target_site_id ?? 0}-${reason.code}`}><span><strong>{reason.target_name || '本站'} · {reason.code}</strong><small>{reason.message}</small></span><b>{reason.count.toLocaleString('zh-CN')} 条</b>{reason.samples?.[0] && <code>{costResolutionSampleText(reason.samples[0])}</code>}</div>)}
				{quality.cost_resolution_observed_at && <small>最近回填：{new Date(quality.cost_resolution_observed_at).toLocaleString('zh-CN')}{quality.cost_resolution_truncated ? '；本轮达到安全上限，后续会继续处理' : ''}</small>}
			</div></details>}
			<div className="profit-next-action"><Info size={17} /><div><strong>你现在可以怎么做</strong><span>{quality.next_action}</span>{quality.operating_estimate_basis && <small>{quality.operating_estimate_basis}</small>}<small>平均利润率 =（已匹配用户扣费 - 已匹配请求级采购成本）/ 已匹配用户扣费；管理员过滤同时作用于这个分子和分母。运营差额另用包含管理员的全站扣费，充值流水不会增加 procurement_cost。</small></div></div>
		</section>
		<div className="operating-detail-grid">
			<section className="operating-detail-panel"><header><div><strong>上游余额与消耗</strong><span>每个站点先应用一次充值折算系数，再按当前 CNY/USD 1:1 汇总</span></div></header>
				{operations.supplier_positions.length === 0 ? <p className="financial-empty-line">尚无上游余额样本。</p> : <div className="operating-supplier-list">{operations.supplier_positions.map((item) => <article key={item.supplier_id}>
					<span><strong>{item.supplier_name}</strong><small>充值折算 × {item.recharge_factor}{item.runway_hours ? ` · 续航 ${formatRunway(Number(item.runway_hours))}` : ''}</small></span>
					<span><small>余额</small><b>{unifiedMoney(item.balance)}</b></span><span><small>近 24h 消耗</small><b>{unifiedMoney(item.daily_spend)}</b></span>
					<details><summary>原始证据</summary><code>{item.raw_balance || '未知'} / {item.raw_daily_spend || '未知'} {item.raw_currency || ''} · {item.conversion_formula || '无换算公式'}</code></details>
				</article>)}</div>}
			</section>
			<section className="operating-detail-panel"><header><div><strong>分组经营贡献</strong><span>按今日用户消耗排序；利润只在请求级采购成本完整时显示</span></div></header>
				{operations.groups.length === 0 ? <p className="financial-empty-line">今日尚无可归类的分组调用。</p> : <div className="operating-group-list">{operations.groups.map((item) => <article key={item.group}>
					<div><strong>{item.group}</strong><small>{item.requests.toLocaleString('zh-CN')} 次请求 · 收入覆盖 {(Number(item.revenue_coverage) * 100).toFixed(1)}% · 成本覆盖 {(Number(item.cost_coverage) * 100).toFixed(1)}%</small></div>
					<div className="operating-group-track"><span style={{ width: `${Math.max(2, item.requests / maximumGroupRequests * 100)}%` }} /></div>
					<span><b>{unifiedMoney(item.revenue)}</b><small>用户消耗</small></span><span><b>{unifiedMoney(item.gross_profit)}</b><small>毛利 · {unifiedRatio(item.gross_margin)}</small></span>
				</article>)}</div>}
			</section>
		</div>
		{operations.warnings.length > 0 && <div className="intelligence-warning-list operating-warnings">{operations.warnings.map((warning) => <span key={warning}><TriangleAlert size={15} />{warning}</span>)}</div>}
	</>;
}

function recommendationHealth(status: string) {
	if (status === 'applied' || status === 'no_change') return 'healthy' as const;
	if (status === 'pending' || status === 'applying' || status === 'stale') return 'warning' as const;
	if (status === 'failed') return 'error' as const;
	return 'unknown' as const;
}

function outcomeTone(status: string) {
	if (status === 'observed') return 'healthy' as const;
	if (['awaiting_ledger', 'not_due', 'coverage_unknown', 'insufficient_coverage'].includes(status)) return 'warning' as const;
	return 'unknown' as const;
}

function ForecastTable({ forecasts }: { forecasts: IntelligenceHorizonForecast[] }) {
	return <div className="table-wrap intelligence-table-wrap">
		<table className="intelligence-forecast-table">
			<thead><tr><th>窗口</th><th>批准结果投影</th><th>维持现状反事实</th><th>保守下界 / 基准 / 上界</th><th>正向评分</th><th>实绩核对</th></tr></thead>
			<tbody>{forecasts.map((forecast) => {
				const projection = forecast.adaptive_contribution ?? forecast.approve.contribution;
				return <tr key={forecast.hours}>
				<td><strong>{forecast.label}</strong>{forecast.due_at && <small>{new Date(forecast.due_at).toLocaleString('zh-CN')}</small>}</td>
				<td className="mono"><strong>{compact(projection.base ?? projection.p50)}</strong>{forecast.learning && <small title={forecast.learning.note}>{forecast.learning.active ? `结构值 ${compact(forecast.approve.contribution.base)} · ${forecast.learning.independent_windows} 个不重叠窗口校正` : `${forecast.learning.independent_windows}/${forecast.learning.minimum_windows} 个不重叠窗口，保持先验`}</small>}</td>
				<td className="mono">{compact(forecast.reject.contribution.base ?? forecast.reject.contribution.p50)}</td>
				<td className="mono interval-cell"><span>{compact(forecast.delta_contribution.low ?? forecast.delta_contribution.p10)}</span><strong>{compact(forecast.delta_contribution.base ?? forecast.delta_contribution.p50)}</strong><span>{compact(forecast.delta_contribution.high ?? forecast.delta_contribution.p90)}</span></td>
				<td title="由未校准敏感性带计算的方向评分，不是统计概率">{percent(forecast.positive_score)}</td>
				<td><StatusBadge status={outcomeTone(forecast.outcome_status)} label={outcomeLabels[forecast.outcome_status] ?? forecast.outcome_status} />{forecast.observation && <small>同口径实绩 {compact(forecast.observation.contribution)} {forecast.observation.unit} · 覆盖 {percent(forecast.observation.coverage_ratio)}</small>}</td>
			</tr>;
			})}</tbody>
		</table>
	</div>;
}

function RecommendationCard({ item, onOpenChanges }: { item: IntelligenceRecommendation; onOpenChanges: () => void }) {
	const [expanded, setExpanded] = useState(item.status === 'pending');
	const forecasts = Array.isArray(item.forecasts) ? item.forecasts : [];
	const evidenceItems = Array.isArray(item.evidence) ? item.evidence : [];
	const warningItems = Array.isArray(item.warnings) ? item.warnings : [];
	const isContract = item.pricing_mode === 'group_model_contract';
	const cashForecast = ['ledger_derived_cash_contribution_v1', 'contract_planned_cash_contribution_v1'].includes(forecasts[0]?.metric_basis ?? '');
	const forecastUnit = forecasts[0]?.approve?.contribution?.unit;
	return <article className={`intelligence-recommendation ${expanded ? 'expanded' : ''}`}>
		<button className="intelligence-recommendation-head" onClick={() => setExpanded((value) => !value)}>
			<span className="intelligence-policy-icon"><Scale size={19} /></span>
			<span className="intelligence-policy-title"><strong>{item.policy_name}</strong><small>{item.supplier} → {item.target} · {item.target_group}</small></span>
			<span className="ratio-change"><small>{isContract ? '逐模型计价' : '倍率'}</small><strong>{isContract ? `${item.forecast_total_models ?? 0} 个模型` : <>{item.current_ratio || '新建'} <ArrowRight size={14} /> {item.proposed_ratio}</>}</strong></span>
			<StatusBadge status={recommendationHealth(item.status)} label={lifecycleLabels[item.lifecycle_status] ?? item.lifecycle_status} />
		</button>
		{expanded && <div className="intelligence-recommendation-body">
			<div className="intelligence-evidence-strip">{evidenceItems.map((evidence) => <div key={evidence.key} className={`evidence-chip evidence-${evidence.level}`} title={evidence.note}>
				<span>{evidence.label}</span><b>{evidenceLabels[evidence.level]}</b><strong>{evidence.value ? `${compact(evidence.value)}${evidence.unit ? ` ${evidence.unit}` : ''}` : evidence.source}</strong>
			</div>)}</div>
			{isContract && <div className="intelligence-contract-evidence"><div><span>当前合同</span><code title={item.current_revision}>{item.current_revision ? `${item.current_revision.slice(0, 12)}…` : '无独立合同'}</code></div><ArrowRight size={16} /><div><span>建议合同</span><code title={item.proposed_revision}>{item.proposed_revision ? `${item.proposed_revision.slice(0, 12)}…` : '待生成'}</code></div><div><span>收益预测覆盖</span><strong>{item.forecast_coverage || `${item.forecast_covered_models ?? 0}/${item.forecast_total_models ?? 0}`}</strong></div></div>}
			{item.forecast_ready && forecasts.length > 0 ? <>
				<div className="forecast-heading"><div><strong>批准情景与维持现状反事实</strong><span>{isContract ? `按每个模型各自的当前售价、建议售价、采购成本和真实需求计算，单位统一为 ${forecastUnit || '合同币种'}。` : cashForecast ? `按 ${forecastUnit} 同模型同单位账本换算；结果窗口从远程读回生效时点开始。` : '当前显示无币种的倍率加权等价贡献，不与现金账本混算。'} 下界/基准/上界是保守敏感性带，正向评分不是统计概率。</span></div><small>模型 {item.model_version} · 输入 {item.input_hash.slice(0, 10)}</small></div>
				<ForecastTable forecasts={forecasts} />
			</> : <div className="intelligence-unavailable"><Info size={18} /><div><strong>暂不生成收益数字</strong><span>{item.forecast_reason || '证据不足'}</span></div></div>}
			{warningItems.length > 0 && <div className="intelligence-warning-list">{warningItems.map((warning) => <span key={warning}><TriangleAlert size={15} />{warning}</span>)}</div>}
			<div className="intelligence-card-actions"><span>{item.effective_at ? `远程生效于 ${new Date(item.effective_at).toLocaleString('zh-CN')}` : `建议生成于 ${new Date(item.created_at).toLocaleString('zh-CN')}`}</span>{item.status === 'pending' && <Button onClick={onOpenChanges}><Check size={16} />前往审批</Button>}</div>
		</div>}
	</article>;
}

type IntelligencePageProps = { onNavigate: (page: PageId) => void; onNotify: (message: string, tone?: 'success' | 'info' | 'error') => void };
const recommendationPageSize = 20;

export function IntelligencePage(props: IntelligencePageProps) {
	return <IntelligencePageBoundary><IntelligencePageContent {...props} /></IntelligencePageBoundary>;
}

function IntelligencePageContent({ onNavigate, onNotify }: IntelligencePageProps) {
	const [data, setData] = useState<IntelligenceDashboard | null>(null);
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState('');
	const [settingsOpen, setSettingsOpen] = useState(false);
	const [saving, setSaving] = useState(false);
	const [scopeSaving, setScopeSaving] = useState(false);
	const [draft, setDraft] = useState<IntelligenceConfig | null>(null);
	const [filter, setFilter] = useState<'active' | 'all' | 'tracking'>('active');
	const [page, setPage] = useState(1);

	const load = async () => {
		setLoading(true); setError('');
		try {
			const result = await api.intelligence();
			const normalized = normalizeIntelligenceDashboard(result.data);
			setData(normalized); setDraft(normalized.config);
		} catch (reason) { setError(getErrorMessage(reason)); }
		finally { setLoading(false); }
	};

	useEffect(() => { void load(); }, []);

	const items = useMemo(() => {
		if (!data) return [];
		if (filter === 'active') return data.recommendations.filter((item) => ['pending', 'applying'].includes(item.status));
		if (filter === 'tracking') return data.recommendations.filter((item) => item.lifecycle_status === 'tracking');
		return data.recommendations;
	}, [data, filter]);
	const pageCount = Math.max(1, Math.ceil(items.length / recommendationPageSize));
	const visibleItems = useMemo(() => items.slice((page - 1) * recommendationPageSize, page * recommendationPageSize), [items, page]);
	useEffect(() => { setPage(1); }, [filter]);
	useEffect(() => { if (page > pageCount) setPage(pageCount); }, [page, pageCount]);

	const saveConfig = async () => {
		if (!draft) return;
		setSaving(true);
		try {
			await api.updateIntelligenceConfig(draft);
			await load(); setSettingsOpen(false); onNotify('经营模型参数已热更新并重新计算', 'success');
		} catch (reason) { onNotify(getErrorMessage(reason), 'error'); }
		finally { setSaving(false); }
	};

	const toggleAdministratorScope = async () => {
		if (!data || scopeSaving) return;
		setScopeSaving(true);
		try {
			const next = {
				...data.config,
				ledger_exclusions: { ...data.config.ledger_exclusions, exclude_administrators: !data.operations.admin_excluded },
			};
			await api.updateIntelligenceConfig(next);
			await load();
			onNotify(next.ledger_exclusions.exclude_administrators ? '经营数据已恢复默认口径：排除管理员' : '经营数据已切换为包含管理员', 'success');
		} catch (reason) { onNotify(getErrorMessage(reason), 'error'); }
		finally { setScopeSaving(false); }
	};

	if (loading && !data) return <div className="inline-loading intelligence-loading"><RefreshCw size={20} className="spin" />正在核对经营证据和结果窗口…</div>;
	if (error && !data) return <ErrorState message={error} onRetry={() => void load()} />;
	if (!data) return null;
	const exclusions: LedgerExclusionPolicy = draft?.ledger_exclusions ?? {
		exclude_administrators: true, exclude_test_users: true, exclude_internal_tokens: true,
	};
	const updateExclusions = (next: Partial<LedgerExclusionPolicy>) => {
		if (!draft) return;
		setDraft({ ...draft, ledger_exclusions: { ...exclusions, ...next } });
	};

	return <>
		<PageHeader eyebrow="经营决策" title="经营智能" description="把上游成本、本站价格、真实用量与执行结果放在同一证据链中。每条建议同时展示批准情景与维持现状反事实，真实账本和模型估算始终分开。" actions={<>
			<Button variant="secondary" onClick={() => void toggleAdministratorScope()} disabled={scopeSaving}><ShieldCheck size={16} />{scopeSaving ? '切换中' : data.operations.admin_excluded ? '当前排除管理员' : '当前包含管理员'}</Button>
			<Button variant="secondary" onClick={() => setSettingsOpen(true)}><Settings2 size={16} />模型参数</Button>
			<Button onClick={() => void load()} disabled={loading}><RefreshCw size={16} className={loading ? 'spin' : ''} />重新计算</Button>
		</>} />

		{data.warnings.length > 0 && <div className="intelligence-notice"><ShieldCheck size={20} /><div><strong>当前结论边界</strong>{data.warnings.map((warning) => <span key={warning}>{warning}</span>)}</div></div>}

		<Section title="统一经营现金快照" description={`今日从 ${new Date(data.operations.day_started_at).toLocaleString('zh-CN')} 起；统一按 ${data.operations.display_currency} 展示，原币种、折算系数和未知项均保留`} action={<StatusBadge status={data.operations.admin_excluded ? 'healthy' : 'warning'} label={data.operations.admin_excluded ? '已排除管理员' : '已包含管理员'} />}>
			<OperatingCashOverview operations={data.operations} />
		</Section>

		<div className="metric-grid intelligence-metrics">
			<article className="metric-card accent-teal"><div className="metric-icon"><BrainCircuit size={20} /></div><div className="metric-label">建议 / 可预测</div><div className="metric-value">{data.summary.forecast_ready}<small> / {data.summary.recommendations}</small></div><div className="metric-foot">证据完整才生成数字</div></article>
			<article className="metric-card accent-amber"><div className="metric-icon"><Activity size={20} /></div><div className="metric-label">待审批 / 跟踪中</div><div className="metric-value">{data.summary.pending}<small> / {data.summary.applied}</small></div><div className="metric-foot">决策与结果窗口分离</div></article>
			<article className="metric-card accent-blue"><div className="metric-icon"><Database size={20} /></div><div className="metric-label">窗口内导入记录</div><div className="metric-value">{data.financials.raw_event_count}</div><div className="metric-foot">纳入当前口径 {data.financials.included_event_count} · 排除 {data.financials.excluded_event_count}</div></article>
			<article className="metric-card accent-violet"><div className="metric-icon"><Gauge size={20} /></div><div className="metric-label">模型校准</div><div className="metric-value">{data.calibration.decision_count}<small> / {data.calibration.minimum_samples}</small></div><div className={`metric-foot ${data.calibration.ready ? 'positive' : 'warning'}`}>{data.calibration.ready ? `WAPE ${percent(data.calibration.wape)} · ${data.calibration.mape_sample_count} 个独立决策` : `${data.calibration.sample_count} 个结果窗口，按 horizon 分层学习`}</div></article>
		</div>

		<Section title="经营现金流与单位经济" description="充值现金流与请求级收入、采购成本分别统计；每种币种独立展示，未知字段不会按 0 参与利润计算" action={<StatusBadge status={coverageTone(data.financials.coverage.status)} label={`${data.financials.included_event_count}/${data.financials.raw_event_count} 个事件纳入`} />}>
			<FinancialOverview financials={data.financials} />
		</Section>

		<Section title="决策建议" description="按真实变更建议生成双情景预测；审批仍在变更中心完成，避免两套写入入口" action={<div className="segmented intelligence-filter">{([['active', '待处理'], ['tracking', '结果跟踪'], ['all', '全部']] as const).map(([value, label]) => <button key={value} className={filter === value ? 'active' : ''} onClick={() => setFilter(value)}>{label}</button>)}</div>}>
			{items.length === 0 ? <EmptyState icon={<Check size={24} />} title="当前分类没有建议" description={filter === 'active' ? '只有真正可操作的等待决策或执行中建议会出现在这里；已过期记录保留在“全部”审计中。' : '选择“全部”查看历史建议。'} /> : <><div className="intelligence-recommendations">{visibleItems.map((item) => <RecommendationCard key={item.proposal_id} item={item} onOpenChanges={() => onNavigate('changes')} />)}</div>{pageCount > 1 && <div className="intelligence-pagination"><span>第 {page} / {pageCount} 页 · 共 {items.length} 条</span><div><Button variant="secondary" disabled={page <= 1} onClick={() => setPage((value) => Math.max(1, value - 1))}>上一页</Button><Button variant="secondary" disabled={page >= pageCount} onClick={() => setPage((value) => Math.min(pageCount, value + 1))}>下一页</Button></div></div>}</>}
		</Section>

		<div className="intelligence-lower-grid">
			<Section title="证据账本" description="计算采用的证据等级和当前覆盖范围">
				<div className="evidence-ledger-summary"><div><CircleDollarSign size={18} /><span>精确收入事件</span><strong>{data.ledger.exact_revenue_events}</strong></div><div><Database size={18} /><span>精确采购成本</span><strong>{data.ledger.exact_cost_events}</strong></div><div><Clock3 size={18} /><span>最近事件</span><strong>{data.ledger.latest_at ? new Date(data.ledger.latest_at).toLocaleString('zh-CN') : '尚未接入'}</strong></div></div>
				<div className="evidence-legend">{data.evidence_legend.map((item) => <div key={item.key}><b className={`evidence-dot evidence-${item.level}`} /> <strong>{item.label}</strong><span>{item.note}</span></div>)}</div>
			</Section>
			<Section title="校准与学习" description="只用实际执行、证据完整且计量口径一致的批准场景；维持现状反事实不进入校准">
				<div className="calibration-panel"><div className="calibration-ring" style={{ '--coverage': `${Math.min(100, Math.max(0, (numeric(data.calibration.band_coverage) ?? 0) * 100))}%` } as CSSProperties}><strong>{data.calibration.ready ? percent(data.calibration.band_coverage) : data.calibration.decision_count}</strong><span>{data.calibration.ready ? '保守带命中率' : '有效决策'}</span></div><div><StatusBadge status={data.calibration.ready ? 'healthy' : 'warning'} label={data.calibration.ready ? '各 horizon 已达门槛' : '分层学习中'} /><p>{data.calibration.note}</p>{data.calibration.mae && <small>MAE {compact(data.calibration.mae)} · WAPE {percent(data.calibration.wape)} · sMAPE {percent(data.calibration.smape)}；近零排除 {data.calibration.near_zero_excluded}，覆盖不足 {data.calibration.coverage_excluded}，截断 {data.calibration.censored_count}</small>}</div></div>
			</Section>
		</div>

		<Modal open={settingsOpen && !!draft} onClose={() => !saving && setSettingsOpen(false)} title="经营模型参数" description="参数保存后立即生效并重新计算，不需要重启服务。先验只控制不确定性与价格响应，精确账本保持原值。" footer={<><Button variant="ghost" onClick={() => setSettingsOpen(false)} disabled={saving}>取消</Button><Button onClick={() => void saveConfig()} disabled={saving}><Save size={16} />{saving ? '保存中' : '保存并重算'}</Button></>}>
			{draft && <div className="intelligence-settings-grid">
				<label className="field"><span>价格弹性先验</span><input value={draft.price_elasticity} onChange={(event) => setDraft({ ...draft, price_elasticity: event.target.value })} /><small>-5 到 0；负数表示涨价后需求下降，默认 -0.35</small></label>
				<label className="field"><span>先验不确定性</span><input value={draft.prior_uncertainty} onChange={(event) => setDraft({ ...draft, prior_uncertainty: event.target.value })} /><small>0 到 2；仅有单一时间窗或低置信用量时使用</small></label>
				<label className="field"><span>观测不确定性</span><input value={draft.observed_uncertainty} onChange={(event) => setDraft({ ...draft, observed_uncertainty: event.target.value })} /><small>0 到 2；7 天和 30 天高置信用量齐全时使用</small></label>
				<label className="field"><span>最小校准样本</span><input type="number" min={3} max={1000} value={draft.minimum_calibration_samples} onChange={(event) => setDraft({ ...draft, minimum_calibration_samples: Number(event.target.value) })} /><small>达到门槛前不会宣称模型精度稳定</small></label>
				<label className="field"><span>预测现金币种</span><input value={draft.forecast_currency ?? ''} placeholder="例如 CNY；留空使用等价贡献" maxLength={3} onChange={(event) => setDraft({ ...draft, forecast_currency: event.target.value.toUpperCase() })} /><small>仅在现金折算因子与账本使用同一币种时填写；填写后才启用现金实绩校准</small></label>
				<label className="field"><span>经营展示币种</span><input value={draft.display_currency} maxLength={3} onChange={(event) => setDraft({ ...draft, display_currency: event.target.value.toUpperCase() })} /><small>首页和经营详情共用；默认 CNY</small></label>
				<label className="field"><span>CNY / USD 展示汇率</span><div className="ledger-sync-paging"><input value={draft.currency_rates.CNY ?? '1'} aria-label="CNY 汇率" onChange={(event) => setDraft({ ...draft, currency_rates: { ...draft.currency_rates, CNY: event.target.value } })} /><input value={draft.currency_rates.USD ?? '1'} aria-label="USD 汇率" onChange={(event) => setDraft({ ...draft, currency_rates: { ...draft.currency_rates, USD: event.target.value } })} /></div><small>当前按 1:1 合并；需要真实汇率时可在线调整</small></label>
				<label className="field"><span>经营日时区</span><input value={draft.business_time_zone} onChange={(event) => setDraft({ ...draft, business_time_zone: event.target.value })} /><small>默认 Asia/Hong_Kong，用于“今日”起止时间</small></label>
				<label className="field"><span>最低联合账本覆盖率</span><input value={draft.minimum_ledger_coverage} onChange={(event) => setDraft({ ...draft, minimum_ledger_coverage: event.target.value })} /><small>0 到 1；默认 0.95，收入与成本必须同时覆盖</small></label>
				<label className="field"><span>相对误差近零门槛</span><input value={draft.relative_error_floor} onChange={(event) => setDraft({ ...draft, relative_error_floor: event.target.value })} /><small>低于该绝对金额不计 MAPE，仍计 MAE 与 sMAPE</small></label>
				<label className="field"><span>现金换算回看天数</span><input type="number" min={7} max={365} value={draft.cash_lookback_days} onChange={(event) => setDraft({ ...draft, cash_lookback_days: Number(event.target.value) })} /><small>只使用决策前的同模型、同单位历史账本</small></label>
				<label className="field"><span>经营统计回看天数</span><input type="number" min={1} max={3660} value={draft.financial_lookback_days} onChange={(event) => setDraft({ ...draft, financial_lookback_days: Number(event.target.value) })} /><small>控制现金流、毛利与用户分群的页面统计窗口</small></label>
				<div className="intelligence-exclusion-settings full"><strong>经营口径排除</strong><span>只影响报表和学习，不删除原始账本。默认排除管理员、测试账号和内部令牌。</span>
					<label className="check-row"><input type="checkbox" checked={exclusions.exclude_administrators} onChange={(event) => updateExclusions({ exclude_administrators: event.target.checked })} /><span>排除管理员调用</span></label>
					<label className="check-row"><input type="checkbox" checked={exclusions.exclude_test_users} onChange={(event) => updateExclusions({ exclude_test_users: event.target.checked })} /><span>排除测试账号</span></label>
					<label className="check-row"><input type="checkbox" checked={exclusions.exclude_internal_tokens} onChange={(event) => updateExclusions({ exclude_internal_tokens: event.target.checked })} /><span>排除内部令牌</span></label>
				</div>
				<label className="field full"><span>额外排除的用户段</span><input value={(exclusions.excluded_user_segments ?? []).join(', ')} placeholder="例如 internal, classroom-test" onChange={(event) => updateExclusions({ excluded_user_segments: splitList(event.target.value) })} /><small>用逗号或空格分隔；名称规范化后去重</small></label>
				<label className="field"><span>额外排除的用户 ID</span><input value={(exclusions.excluded_user_ids ?? []).join(', ')} placeholder="例如 1, 42" onChange={(event) => updateExclusions({ excluded_user_ids: splitList(event.target.value) })} /></label>
				<label className="field"><span>额外排除的令牌 ID</span><input value={(exclusions.excluded_token_ids ?? []).join(', ')} placeholder="例如 8, 12" onChange={(event) => updateExclusions({ excluded_token_ids: splitList(event.target.value) })} /></label>
			</div>}
		</Modal>
	</>;
}
