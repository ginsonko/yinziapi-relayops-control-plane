import { useEffect, useMemo, useRef, useState } from 'react';
import { Activity, AlertTriangle, ArchiveRestore, ArrowRight, Check, ChevronDown, ChevronLeft, ChevronRight, ChevronUp, CircleHelp, ExternalLink, Eye, Filter, KeyRound, Landmark, LoaderCircle, PackageCheck, PackagePlus, RefreshCw, Search, ShieldCheck, Sparkles, Trash2, TrendingUp, Workflow, X } from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { Activity as ActivityRecord, BalanceForecast, CatalogGroup, CatalogSnapshot, ChangeProposal, Discovery, ListingChangeSet, ListingPlan, OpportunityClassification, OpportunityResponse, OpportunityResult, PolicyMode, RawCredential, RawSupplier, TargetSite } from '../api/types';
import { Button, EmptyState, formatDateTime, formatMoney, formatRunway, MiniBars, Modal, PageHeader, Section, StatusBadge } from '../components/ui';
import { preferredEnabledTarget } from '../data/targets';
import { catalogFreshnessWindowMillis, catalogFutureToleranceMillis, isCatalogEvidenceFresh } from './catalogListing';
import { normalizeResolvedListingSource, parseCanonicalSupplierID, projectListingPreviewIdentity, resolveCatalogGroupIdentity, resolveSupplierIdentity, runAfterCanonicalGroupToken, runAfterCanonicalSupplierID, verifyCatalogSnapshotOwnership } from './listingSourceIdentity';
import type { ListingIdentityFailureStatus, ResolvedListingSource as ResolvedListingSourceIdentity } from './listingSourceIdentity';
import { upstreamKeyManagementLinks } from './upstreamKeyManagement';
import { newIdempotencyKey } from '../idempotency';

export function BalancesPage({ balances }: { balances: BalanceForecast[] }) {
  const [threshold, setThreshold] = useState(48);
	const currencyTotals = balances.reduce<Record<string, number>>((totals, item) => {
		if (item.balance === null || !item.currency) return totals;
		const rawCurrency = item.currency.toUpperCase();
		const currency = rawCurrency === 'CNY' || rawCurrency === 'USD' ? 'CNY/USD' : rawCurrency;
		totals[currency] = (totals[currency] ?? 0) + item.balance;
		return totals;
	}, {});
	const spendTotals = balances.reduce<Record<string, number>>((totals, item) => {
		if (item.dailyAverage === null || !item.currency) return totals;
		const rawCurrency = item.currency.toUpperCase();
		const currency = rawCurrency === 'CNY' || rawCurrency === 'USD' ? 'CNY/USD' : rawCurrency;
		totals[currency] = (totals[currency] ?? 0) + item.dailyAverage;
		return totals;
	}, {});
	const displayCash = (value: number | null, currency = '') => {
		if (value === null) return '待采样';
		const normalized = currency.toUpperCase();
		return normalized === 'CNY' || normalized === 'USD' || normalized === 'CNY/USD'
			? `¥/$ ${value.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`
			: formatMoney(value, normalized);
	};
	const formatTotals = (totals: Record<string, number>) => Object.entries(totals).map(([currency, value]) => displayCash(value, currency)).join(' + ') || '待采样';
  const urgent = balances.filter((item) => item.runwayHours !== null && item.runwayHours < threshold).length;

  return (
    <>
      <PageHeader eyebrow="资金安全" title="余额与续航" description="根据近期实际消耗预测余额可用时间，让充值发生在断供之前。" />
      <div className="balance-hero">
		<div><span>已知上游现金余额</span><strong>{formatTotals(currencyTotals)}</strong><small>已应用一次充值折算系数；快跑配置 0.4 时按原始额度的 40% 计入</small></div>
		<div><span>近 24h 现金消耗</span><strong>{formatTotals(spendTotals)}</strong><small>{balances.some((item) => item.samples.length > 1) ? '按余额历史和最新用量证据估算' : '当前使用最新日耗样本'}</small></div>
        <div className={urgent > 0 ? 'has-alert' : ''}><span>续航预警</span><strong>{urgent}</strong><small>低于 {threshold} 小时</small></div>
        <label><span>预警阈值</span><select value={threshold} onChange={(event) => setThreshold(Number(event.target.value))}><option value={24}>24 小时</option><option value={48}>48 小时</option><option value={72}>72 小时</option><option value={168}>7 天</option></select></label>
      </div>
      <Section title="续航预测" description="异常和临界站点已排在前面">
        <div className="balance-list">
		  {[...balances].sort((a, b) => (a.runwayHours ?? Number.POSITIVE_INFINITY) - (b.runwayHours ?? Number.POSITIVE_INFINITY)).map((item) => {
			const displayStatus = item.runwayHours === null ? 'unknown' : item.runwayHours < threshold * .6 ? 'error' : item.runwayHours < threshold ? 'warning' : 'healthy';
			const progress = item.runwayHours === null ? 0 : Math.min(100, item.runwayHours / Math.max(threshold * 2, 1) * 100);
            return (
              <article className="balance-row" key={item.id}>
				<div className="balance-name"><span className={`balance-icon status-${displayStatus}`}><Landmark size={18} /></span><div><strong>{item.supplier}</strong><StatusBadge status={displayStatus} label={displayStatus === 'healthy' ? '余额充足' : displayStatus === 'warning' ? '建议充值' : displayStatus === 'error' ? '即将用尽' : '等待日耗样本'} /></div></div>
				<div className="balance-number"><span>当前现金余额</span><strong>{displayCash(item.balance, item.currency)}</strong></div>
				<div className="balance-number balance-usage"><span>24h 现金消耗 / 预测日耗</span><strong>{displayCash(item.spentToday, item.currency)} <small>/ {displayCash(item.dailyAverage, item.currency)}</small></strong><small>{item.dailyRequestCount === undefined ? '24h 请求数待采样' : `24h ${item.dailyRequestCount.toLocaleString('zh-CN')} 次请求`}</small></div>
				{item.samples.length > 1 ? <MiniBars values={item.samples} tone={displayStatus === 'error' ? 'red' : displayStatus === 'warning' ? 'amber' : 'teal'} /> : <span className="sample-note">单次样本</span>}
				<div className="runway"><div><span>预计续航</span><strong>{formatRunway(item.runwayHours)}</strong></div><div className="runway-track"><span style={{ width: `${progress}%` }} /></div><small>{item.runwayHours === null ? '取得日耗样本后自动计算' : item.runwayHours >= 720 ? '按当前消耗，30 天内无需充值' : `预计用尽：${formatDateTime(item.forecastAt)}`}</small>{(item.rawValue || item.formula || item.usageSource || item.rawBalance) && <details className="balance-evidence"><summary>查看余额与用量证据</summary><div><span>原始字段</span><code>{item.sourceField || '未记录'}</code><span>采集原始值 / 单位</span><code>{item.rawValue || '未知'} {item.rawUnit || ''}</code><span>原始余额 / 日耗</span><code>{item.rawBalance || '未知'} / {item.rawDailySpend || '未知'} {item.rawCurrency || ''}</code><span>充值折算系数</span><code>{item.cashFactor || '1'}</code><span>换算除数</span><code>{item.divisor || '无需换算'}</code><span>采集归一化</span><code>{item.formula || '上游直接返回金额'}</code><span>现金折算</span><code>{item.conversionFormula || 'cash = raw_amount x 1'}</code><span>历史现金总消耗</span><code>{item.historicalSpend === undefined || item.historicalSpend === null ? '待采样' : displayCash(item.historicalSpend, item.currency)}</code><span>累计请求数</span><code>{item.requestCount === undefined ? '待采样' : item.requestCount.toLocaleString('zh-CN')}</code><span>24h 用量来源</span><code>{item.usageSource || '待采样'}</code><span>余额 / 用量采样</span><code>{formatDateTime(item.observedAt)} / {formatDateTime(item.usageObservedAt)}</code></div></details>}</div>
              </article>
            );
          })}
        </div>
      </Section>
    </>
  );
}

const opportunityLabels: Record<OpportunityClassification, string> = {
	recommended: '建议上架',
	watch: '持续观察',
	no_advantage: '暂无优势',
	insufficient_data: '数据不足',
};

function opportunityMoney(value?: string, currency?: string) {
	if (value === undefined || value === '') return '待计算';
	const parsed = Number(value);
	if (!Number.isFinite(parsed)) return value;
	return `${currency || '计价单位'} ${parsed.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 4 })}`;
}

function opportunityPercent(value?: string) {
	if (value === undefined || value === '') return '待采样';
	const parsed = Number(value);
	return Number.isFinite(parsed) ? `${(parsed * 100).toFixed(1)}%` : value;
}

function opportunityQuantity(value: string, unit?: string) {
	const parsed = Number(value);
	const amount = Number.isFinite(parsed) ? parsed.toLocaleString('zh-CN', { maximumFractionDigits: 4 }) : value;
	return `${amount} ${opportunityUnitLabel(unit)}/日`;
}

function opportunityEvidenceLabel(value: string) {
	if (value === 'observed') return '本站实测';
	if (value === 'prior') return '先验估算';
	return '证据未知';
}

function OpportunityMetricLabel({ label, help }: { label: string; help: string }) {
	return <span className="opportunity-metric-label">{label}<span className="opportunity-metric-help" role="img" aria-label={`${label}说明：${help}`} title={help}><CircleHelp size={11} aria-hidden="true" /></span></span>;
}

function opportunityTime(value?: string) {
	if (!value) return '未记录';
	const parsed = new Date(value);
	return Number.isNaN(parsed.getTime()) ? '未记录' : parsed.toLocaleString('zh-CN', { hour12: false });
}

function opportunityUnitLabel(value?: string) {
	switch ((value || '').toLowerCase()) {
	case 'million_tokens': return '百万 Token';
	case 'requests': return '请求次数';
	case 'images': return '图片数';
	case 'seconds': return '秒数';
	case 'frames': return '帧数';
	case 'normalized_demand': return '归一化需求';
	case 'mixed': return '混合单位';
	default: return value || '未知单位';
	}
}

function recommendationBasisLabel(value?: string) {
	if (value === 'demand_weighted_current_minimum_sale_ratio_v1') {
		return '按本站近期模型需求、可用性和证据权重，反推各模型当前最低可售倍率的统一执行值';
	}
	return value || '当前采购成本、真实需求、可用性与证据权重';
}

const pricingRuleLabels: Record<PolicyMode, string> = {
	multiply: '乘算倍率',
	add: '固定加值',
	target_margin: '目标毛利率',
	markup: '按成本加成',
	subsidy_rate: '公益补贴率',
	fixed: '固定本站倍率',
};

const listingDimensionLabels: Record<string, string> = {
	per_request: '每次请求',
	per_second: '每秒',
	input_per_1m: '输入 / 百万 Token',
	output_per_1m: '输出 / 百万 Token',
	cache_read_per_1m: '缓存读取 / 百万 Token',
	cache_write_per_1m: '缓存写入 / 百万 Token',
	cache_write_5m_per_1m: '5 分钟缓存写入 / 百万 Token',
	cache_write_1h_per_1m: '1 小时缓存写入 / 百万 Token',
	image_input_per_1m: '图片输入 / 百万 Token',
	audio_input_per_1m: '音频输入 / 百万 Token',
	audio_output_per_1m: '音频输出 / 百万 Token',
};

function listingPercent(value?: string) {
	if (!value) return '待验证';
	const parsed = Number(value);
	return Number.isFinite(parsed) ? `${(parsed * 100).toFixed(2)}%` : value;
}

function listingDecimal(value?: string) {
	if (!value) return '';
	const parsed = Number(value);
	return Number.isFinite(parsed)
		? parsed.toLocaleString('en-US', { useGrouping: false, maximumSignificantDigits: 10 })
		: value;
}

function shortContractRevision(value?: string) {
	if (!value) return '旧站无 revision';
	return value.length > 14 ? `${value.slice(0, 12)}…` : value;
}

function listingExecutionStatus(status: string) {
	return ({ pending: '等待执行', applying: '正在上架', applied: '已上架', failed: '上架失败', stale: '证据冲突',
		rolling_back: '回滚待续', rolled_back: '已安全回滚' } as Record<string, string>)[status] ?? status;
}

function listingExecutionHealth(status: string): 'healthy' | 'warning' | 'error' | 'unknown' {
	if (status === 'applied' || status === 'rolled_back') return 'healthy';
	if (status === 'pending' || status === 'applying' || status === 'rolling_back' || status === 'stale') return 'warning';
	if (status === 'failed') return 'error';
	return 'unknown';
}

type ListingSourceBase = Discovery & {
	sourceObservedAt?: string;
	sourceEffectiveRatio?: string;
};

type ListingSourceSelection =
	| (ListingSourceBase & { sourceKind: 'group'; supplierId?: number | string; groupExternalId?: string; sourceSnapshotHash?: string })
	| (ListingSourceBase & { sourceKind: 'catalog_group'; supplierId: number; groupExternalId: string; sourceSnapshotHash: string; sourceEffectiveRatio: string });

type ResolvedListingSource = ListingSourceSelection & {
	supplierId: number;
	groupExternalId: string;
	resolvedIdentity: ResolvedListingSourceIdentity<RawSupplier, CatalogGroup>;
	resolvedSnapshot: CatalogSnapshot;
};

function sourceIdentityError(subject: '上游站点' | '上游分组', label: string, sourceKind: ListingSourceSelection['sourceKind'], status: ListingIdentityFailureStatus, matchCount: number) {
	if (status === 'ambiguous') return sourceKind === 'catalog_group'
		? `精确${subject}「${label}」出现 ${matchCount} 条重复记录，请重新采集并检查连接器目录。`
		: `${subject}「${label}」匹配到 ${matchCount} 条记录，请改用“从完整上游目录上架”按精确 ID 选择。`;
	if (status === 'ownership_mismatch') return `目录快照归属与${subject}「${label}」不一致，请重新采集并检查连接器目录。`;
	if (status === 'invalid_identifier' || status === 'missing_identifier') return `${subject}「${label}」缺少有效的精确标识，请关闭后重新选择。`;
	return `已找不到选择时的${subject}「${label}」，请关闭后重新选择。`;
}

function discoveryListingSource(item: Discovery): ListingSourceSelection {
	return { ...item, sourceKind: 'group' };
}

export function DiscoveriesPage({ discoveries, onOpenMirror }: { discoveries: Discovery[]; onOpenMirror: () => void }) {
	const [opportunities, setOpportunities] = useState<OpportunityResponse | null>(null);
	const [opportunityLoading, setOpportunityLoading] = useState(true);
	const [opportunityRefreshing, setOpportunityRefreshing] = useState(false);
	const [opportunityError, setOpportunityError] = useState('');
	const [opportunityTargets, setOpportunityTargets] = useState<TargetSite[]>([]);
	const [opportunityTargetId, setOpportunityTargetId] = useState('');
	const [classificationFilter, setClassificationFilter] = useState<'all' | OpportunityClassification>('recommended');
	const [familyFilter, setFamilyFilter] = useState('all');
	const [supplierFilter, setSupplierFilter] = useState('all');
	const [minimumProfit, setMinimumProfit] = useState('');
	const [catalogOpen, setCatalogOpen] = useState(false);
	const [catalogLoading, setCatalogLoading] = useState(false);
	const [catalogError, setCatalogError] = useState('');
	const [catalogSuppliers, setCatalogSuppliers] = useState<RawSupplier[]>([]);
	const [catalogSupplierId, setCatalogSupplierId] = useState('');
	const [catalogSnapshot, setCatalogSnapshot] = useState<CatalogSnapshot | null>(null);
	const [catalogQuery, setCatalogQuery] = useState('');
	const [catalogClock, setCatalogClock] = useState(() => Date.now());
  const [pendingSelection, setPendingSelection] = useState<ListingSourceSelection | null>(null);
  const [selected, setSelected] = useState<ResolvedListingSource | null>(null);
	const [step, setStep] = useState(0);
	const [loading, setLoading] = useState(false);
	const [previewing, setPreviewing] = useState(false);
	const [applying, setApplying] = useState(false);
	const [error, setError] = useState('');
	const [targets, setTargets] = useState<TargetSite[]>([]);
	const [targetGroups, setTargetGroups] = useState<string[]>([]);
	const [targetReadable, setTargetReadable] = useState(false);
	const [targetId, setTargetId] = useState('');
	const [targetMode, setTargetMode] = useState<'new' | 'existing'>('new');
	const [targetGroup, setTargetGroup] = useState('');
	const [pricingMode, setPricingMode] = useState<PolicyMode | 'keep'>('target_margin');
	const [pricingValue, setPricingValue] = useState('0.2');
	const [createGroupMirror, setCreateGroupMirror] = useState(true);
	const [mirrorExecutionMode, setMirrorExecutionMode] = useState<'auto' | 'approval'>('auto');
	const [createPricingMapping, setCreatePricingMapping] = useState(true);
	const [pricingExecutionMode, setPricingExecutionMode] = useState<'auto' | 'approval'>('auto');
	const [recommendedRule, setRecommendedRule] = useState<{ type: PolicyMode; value: string; rounding: string; basis: string; note: string } | null>(null);
	const [allowNegativeProfit, setAllowNegativeProfit] = useState(false);
	const [subsidyBudgetDaily, setSubsidyBudgetDaily] = useState('0');
	const [credentialMode, setCredentialMode] = useState<'existing' | 'dedicated'>('existing');
	const [credentials, setCredentials] = useState<RawCredential[]>([]);
	const [dedicatedKey, setDedicatedKey] = useState('');
	const [channelCredentialId, setChannelCredentialId] = useState<number | null>(null);
	const [selectedModels, setSelectedModels] = useState<string[]>([]);
	const [channelName, setChannelName] = useState('');
	const [channelBaseUrl, setChannelBaseUrl] = useState('');
	const [channelType, setChannelType] = useState('1');
	const [autoBan, setAutoBan] = useState(true);
	const [previewPlan, setPreviewPlan] = useState<ListingPlan | null>(null);
	const [changeSet, setChangeSet] = useState<ListingChangeSet | null>(null);
	const [changeHistory, setChangeHistory] = useState<ListingChangeSet[]>([]);
	const [recentListingChanges, setRecentListingChanges] = useState<ListingChangeSet[]>([]);
	const [recentListingPlans, setRecentListingPlans] = useState<Record<number, ListingPlan>>({});
	const [listingHistoryLoading, setListingHistoryLoading] = useState(true);
	const [listingHistoryError, setListingHistoryError] = useState('');
	const [rollbackCandidate, setRollbackCandidate] = useState<ListingChangeSet | null>(null);
	const [rollbackConfirmed, setRollbackConfirmed] = useState(false);
	const [rollingBackId, setRollingBackId] = useState<number | null>(null);
	const [rollbackError, setRollbackError] = useState('');
	const [previewKey, setPreviewKey] = useState(() => newIdempotencyKey('listing-preview'));
	const [applyKey, setApplyKey] = useState(() => newIdempotencyKey('listing-apply'));
	const catalogRequestGeneration = useRef(0);
	const wizardRequestGeneration = useRef(0);
	const targetRequestGeneration = useRef(0);
	const previewRequestGeneration = useRef(0);
	const applyRequestGeneration = useRef(0);
	const wizardAbortController = useRef<AbortController | null>(null);
	const targetAbortController = useRef<AbortController | null>(null);
	const previewAbortController = useRef<AbortController | null>(null);
	const contractPreview = previewPlan?.group_model_pricing;
	const contractModels = contractPreview?.models ?? [];
	const contractExcludedModels = contractPreview?.excluded_models ?? [];
	const groupVisibilityPreview = contractPreview?.group_visibility;
	const groupVisibilityReceipt = changeSet?.group_model_pricing?.group_visibility;
	const isCatalogSource = selected?.sourceKind === 'catalog_group';
	const isBaseline = (item: ListingSourceSelection) => item.sourceKind === 'group' && item.classification === 'baseline';
	const isUsingRecommendedRule = !!recommendedRule && pricingMode === recommendedRule.type && pricingValue.trim() === recommendedRule.value;
	const sourceSupplier = selected?.resolvedIdentity.supplier ?? null;
	const sourceGroup = selected?.resolvedIdentity.group;
	const sourceSnapshot = selected?.resolvedSnapshot;
	const catalogGroups = useMemo(() => {
		const query = catalogQuery.trim().toLowerCase();
		return [...(catalogSnapshot?.groups ?? [])]
			.filter((group) => !query || group.name.toLowerCase().includes(query) || group.external_id.toLowerCase().includes(query) || group.model_ids.some((model) => model.toLowerCase().includes(query)))
			.sort((left, right) => left.name.localeCompare(right.name, 'zh-CN'));
	}, [catalogQuery, catalogSnapshot]);
	const catalogEvidenceFresh = isCatalogEvidenceFresh(catalogSnapshot, catalogClock);
	const catalogFreshnessMinutes = Math.round(catalogFreshnessWindowMillis / 60_000);
	const sameOrigin = (left: string, right: string) => {
		try { return new URL(left).origin.toLowerCase() === new URL(right).origin.toLowerCase(); } catch { return false; }
	};
	const isChannelAPIKey = (credential: RawCredential) => {
		const kind = credential.kind.toLowerCase();
		return kind.includes('api') && !['login', 'browser', 'cookie', 'session'].some((part) => kind.includes(part)) &&
			(!credential.status || credential.status === 'active') && credential.id !== sourceSupplier?.credential_id &&
			sameOrigin(credential.site_url, channelBaseUrl);
	};
	const channelCredentials = useMemo(() => credentials.filter(isChannelAPIKey), [credentials, sourceSupplier?.credential_id, channelBaseUrl]);
	const loginCredential = credentials.find((credential) => credential.id === sourceSupplier?.credential_id);
	const selectedChannelCredential = channelCredentials.find((credential) => credential.id === channelCredentialId);
	const keyManagementLinks = useMemo(() => upstreamKeyManagementLinks(sourceSupplier), [sourceSupplier]);
	const legacyByID = useMemo(() => new Map(discoveries.map((item) => [item.id, item])), [discoveries]);
	const families = useMemo(() => Array.from(new Set((opportunities?.items ?? []).flatMap((item) => item.model_families))).sort(), [opportunities]);
	const opportunitySuppliers = useMemo(() => Array.from(new Set((opportunities?.items ?? []).map((item) => item.supplier))).sort(), [opportunities]);
	const filteredOpportunities = useMemo(() => {
		const threshold = minimumProfit.trim() === '' ? null : Number(minimumProfit);
		return (opportunities?.items ?? []).filter((item) => {
			if (classificationFilter !== 'all' && item.classification !== classificationFilter) return false;
			if (familyFilter !== 'all' && !item.model_families.includes(familyFilter)) return false;
			if (supplierFilter !== 'all' && item.supplier !== supplierFilter) return false;
			if (threshold !== null && Number.isFinite(threshold) && Number(item.expected_daily_profit ?? Number.NEGATIVE_INFINITY) < threshold) return false;
			return true;
		});
	}, [classificationFilter, familyFilter, minimumProfit, opportunities, supplierFilter]);

	const loadOpportunities = async (refresh = false, targetIdOverride = opportunityTargetId) => {
		if (refresh) setOpportunityRefreshing(true); else setOpportunityLoading(true);
		setOpportunityError('');
		try {
			const parsedTargetId = targetIdOverride.trim() === '' ? undefined : Number(targetIdOverride);
			const result = await api.opportunities(refresh, Number.isFinite(parsedTargetId) ? parsedTargetId : undefined);
			setOpportunities(result.data);
		} catch (cause) {
			setOpportunityError(getErrorMessage(cause));
		} finally {
			setOpportunityLoading(false);
			setOpportunityRefreshing(false);
		}
	};

	const loadListingHistory = async () => {
		setListingHistoryLoading(true);
		setListingHistoryError('');
		try {
			const result = await api.listListingChangeSets(undefined, 20);
			setRecentListingChanges(result.data);
			const planIds = Array.from(new Set(result.data.map((item) => item.plan_id)));
			const planResults = await Promise.allSettled(planIds.map(async (id) => (await api.getListingPlan(id)).data));
			const plans: Record<number, ListingPlan> = {};
			for (const item of planResults) {
				if (item.status === 'fulfilled') plans[item.value.id] = item.value;
			}
			setRecentListingPlans(plans);
		} catch (cause) {
			setListingHistoryError(getErrorMessage(cause));
		} finally {
			setListingHistoryLoading(false);
		}
	};

	useEffect(() => {
		let cancelled = false;
		const initialize = async () => {
			setOpportunityLoading(true);
			setOpportunityError('');
			try {
				const targetResult = await api.listTargets();
				if (cancelled) return;
				const enabledTargets = targetResult.data.filter((target) => target.enabled);
				setOpportunityTargets(enabledTargets);
				const preferred = preferredEnabledTarget(enabledTargets);
				const preferredId = preferred ? String(preferred.id) : '';
				setOpportunityTargetId(preferredId);
				const result = await api.opportunities(false, preferred?.id);
				if (!cancelled) setOpportunities(result.data);
			} catch (cause) {
				if (!cancelled) setOpportunityError(getErrorMessage(cause));
			} finally {
				if (!cancelled) setOpportunityLoading(false);
			}
		};
		void initialize();
		return () => { cancelled = true; };
	}, []);

	useEffect(() => {
		void loadListingHistory();
	}, []);

	useEffect(() => {
		if (!catalogOpen) return undefined;
		setCatalogClock(Date.now());
		const interval = window.setInterval(() => setCatalogClock(Date.now()), 60_000);
		const now = Date.now();
		const observedAt = catalogSnapshot?.evidence?.observed_at ? new Date(catalogSnapshot.evidence.observed_at).getTime() : Number.NaN;
		const deadlineDelay = Number.isFinite(observedAt) && observedAt <= now + catalogFutureToleranceMillis
			? observedAt + catalogFreshnessWindowMillis - now + 1
			: Number.NaN;
		const deadline = Number.isFinite(deadlineDelay) && deadlineDelay > 0
			? window.setTimeout(() => setCatalogClock(Date.now()), deadlineDelay)
			: undefined;
		return () => {
			window.clearInterval(interval);
			if (deadline !== undefined) window.clearTimeout(deadline);
		};
	}, [catalogOpen, catalogSnapshot?.evidence?.observed_at]);

	useEffect(() => {
		if (!opportunityTargetId) return undefined;
		const refresh = () => {
			if (document.visibilityState === 'visible' && !opportunityRefreshing) void loadOpportunities(false);
		};
		const timer = window.setInterval(refresh, 60_000);
		document.addEventListener('visibilitychange', refresh);
		return () => {
			window.clearInterval(timer);
			document.removeEventListener('visibilitychange', refresh);
		};
	}, [opportunityTargetId]);

	const discoveryForOpportunity = (item: OpportunityResult): ListingSourceSelection => ({
		...(legacyByID.get(item.id) ?? {
			id: item.id, supplier: item.supplier, upstreamGroup: item.group, upstreamRate: null,
			modelCount: item.model_ids.length, featuredModels: item.model_ids.slice(0, 4), foundAt: item.observed_at,
			recommendation: item.reason, status: 'new', classification: 'new',
		}),
		sourceKind: 'group',
		supplierId: item.supplier_id,
	});

	const invalidatePreview = () => {
		previewAbortController.current?.abort();
		previewAbortController.current = null;
		previewRequestGeneration.current += 1;
		applyRequestGeneration.current += 1;
		setPreviewPlan(null);
		setChangeSet(null);
		setChangeHistory([]);
		setPreviewKey(newIdempotencyKey('listing-preview'));
		setApplyKey(newIdempotencyKey('listing-apply'));
	};

	const loadCatalogSupplier = async (supplierId: string, existingGeneration?: number, availableSuppliers = catalogSuppliers) => {
		const requestGeneration = existingGeneration ?? ++catalogRequestGeneration.current;
		setCatalogSupplierId(supplierId);
		setCatalogSnapshot(null);
		setCatalogError('');
		setCatalogQuery('');
		if (!supplierId) {
			if (requestGeneration === catalogRequestGeneration.current) setCatalogLoading(false);
			return;
		}
		setCatalogLoading(true);
		try {
			const supplierResolution = resolveSupplierIdentity(availableSuppliers, { supplierId, requireExact: true });
			if (supplierResolution.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', supplierId, 'catalog_group', supplierResolution.status, supplierResolution.matchCount));
			}
			const result = await runAfterCanonicalSupplierID(supplierResolution.value.id, (canonicalID) => api.supplierSnapshot(canonicalID));
			if (result.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', supplierResolution.value.name, 'catalog_group', result.status, result.matchCount));
			}
			if (requestGeneration !== catalogRequestGeneration.current) return;
			const snapshot = result.value.data;
			const ownership = verifyCatalogSnapshotOwnership(snapshot, supplierResolution.value.id);
			if (ownership.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', supplierResolution.value.name, 'catalog_group', ownership.status, ownership.matchCount));
			}
			setCatalogClock(Date.now());
			setCatalogSnapshot(snapshot);
			if (!snapshot.evidence?.response_hash) {
				setCatalogError('最近保存的目录缺少响应哈希，请先重新采集该上游后再上架。');
			}
		} catch (cause) {
			if (requestGeneration === catalogRequestGeneration.current) setCatalogError(getErrorMessage(cause));
		} finally {
			if (requestGeneration === catalogRequestGeneration.current) setCatalogLoading(false);
		}
	};

	const openCatalogPicker = async () => {
		const requestGeneration = ++catalogRequestGeneration.current;
		setCatalogOpen(true);
		setCatalogLoading(true);
		setCatalogError('');
		setCatalogSnapshot(null);
		setCatalogQuery('');
		setCatalogClock(Date.now());
		try {
			const result = await api.listSuppliers();
			if (requestGeneration !== catalogRequestGeneration.current) return;
			const enabled = result.data.filter((supplier) => supplier.enabled).sort((left, right) => left.name.localeCompare(right.name, 'zh-CN'));
			setCatalogSuppliers(enabled);
			if (enabled.length === 0) {
				setCatalogSupplierId('');
				setCatalogError('当前没有启用且可读取目录的上游站点。');
				return;
			}
			const preferred = enabled.find((supplier) => supplier.name === supplierFilter) ?? enabled[0];
			await loadCatalogSupplier(String(preferred.id), requestGeneration, enabled);
		} catch (cause) {
			if (requestGeneration === catalogRequestGeneration.current) setCatalogError(getErrorMessage(cause));
		} finally {
			if (requestGeneration === catalogRequestGeneration.current) setCatalogLoading(false);
		}
	};

	const refreshCatalogSupplier = async () => {
		if (!catalogSupplierId) return;
		const requestGeneration = ++catalogRequestGeneration.current;
		setCatalogLoading(true);
		setCatalogError('');
		try {
			const supplierResolution = resolveSupplierIdentity(catalogSuppliers, { supplierId: catalogSupplierId, requireExact: true });
			if (supplierResolution.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', catalogSupplierId, 'catalog_group', supplierResolution.status, supplierResolution.matchCount));
			}
			const result = await runAfterCanonicalSupplierID(supplierResolution.value.id, (canonicalID) => api.collectSupplier(String(canonicalID)));
			if (result.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', supplierResolution.value.name, 'catalog_group', result.status, result.matchCount));
			}
			if (requestGeneration !== catalogRequestGeneration.current) return;
			await loadCatalogSupplier(catalogSupplierId, requestGeneration);
		} catch (cause) {
			if (requestGeneration === catalogRequestGeneration.current) setCatalogError(getErrorMessage(cause));
		} finally {
			if (requestGeneration === catalogRequestGeneration.current) setCatalogLoading(false);
		}
	};

	const closeCatalogPicker = () => {
		catalogRequestGeneration.current += 1;
		setCatalogOpen(false);
		setCatalogLoading(false);
	};

	const closeWizard = () => {
		if (previewing || applying) return;
		wizardAbortController.current?.abort();
		targetAbortController.current?.abort();
		previewAbortController.current?.abort();
		wizardAbortController.current = null;
		targetAbortController.current = null;
		previewAbortController.current = null;
		wizardRequestGeneration.current += 1;
		targetRequestGeneration.current += 1;
		previewRequestGeneration.current += 1;
		applyRequestGeneration.current += 1;
		setPendingSelection(null);
		setSelected(null);
		setLoading(false);
		setDedicatedKey('');
		setChannelCredentialId(null);
	};

	const openWizard = async (item: ListingSourceSelection, opportunity?: OpportunityResult) => {
		wizardAbortController.current?.abort();
		targetAbortController.current?.abort();
		previewAbortController.current?.abort();
		const abortController = new AbortController();
		wizardAbortController.current = abortController;
		const requestGeneration = ++wizardRequestGeneration.current;
		targetRequestGeneration.current += 1;
		previewRequestGeneration.current += 1;
		applyRequestGeneration.current += 1;
		setPendingSelection(item); setSelected(null); setStep(0); setLoading(true); setPreviewing(false); setApplying(false); setError('');
		setTargets([]); setTargetId(''); setTargetMode('new'); setTargetGroups([]); setTargetReadable(false);
		setSelectedModels([]); setChannelName(''); setChannelBaseUrl(''); setChannelType('1'); setAutoBan(true);
		setPreviewPlan(null); setChangeSet(null); setChangeHistory([]);
		setCredentialMode('existing'); setDedicatedKey(''); setChannelCredentialId(null); setCredentials([]); setAllowNegativeProfit(false); setSubsidyBudgetDaily('0');
		setCreateGroupMirror(true); setMirrorExecutionMode('auto'); setCreatePricingMapping(true); setPricingExecutionMode('auto');
		setPreviewKey(newIdempotencyKey('listing-preview')); setApplyKey(newIdempotencyKey('listing-apply'));
		setTargetGroup(item.upstreamGroup.toLowerCase().replaceAll(' ', '-'));
		if (opportunity?.recommended_rule_type && opportunity.recommended_rule_value) {
			setRecommendedRule({ type: opportunity.recommended_rule_type, value: opportunity.recommended_rule_value,
				rounding: opportunity.recommended_rounding_step || '0.000001', basis: recommendationBasisLabel(opportunity.recommendation_basis),
				note: opportunity.recommendation_note || '使用该规则时，列表中的收益预测才可直接比较。' });
			setPricingMode(opportunity.recommended_rule_type);
			setPricingValue(opportunity.recommended_rule_value);
		} else {
			setRecommendedRule(null);
			setPricingMode('target_margin');
			setPricingValue('0.2');
		}
		const hasExplicitSupplierID = Object.hasOwn(item, 'supplierId') || item.sourceKind === 'catalog_group';
		if (hasExplicitSupplierID) {
			const supplierID = parseCanonicalSupplierID(item.supplierId);
			if (supplierID.status !== 'resolved') {
				setError(sourceIdentityError('上游站点', item.supplier, item.sourceKind, supplierID.status, supplierID.matchCount));
				setLoading(false);
				abortController.abort();
				if (wizardAbortController.current === abortController) wizardAbortController.current = null;
				return;
			}
		}
		const hasExplicitGroupID = Object.hasOwn(item, 'groupExternalId') || item.sourceKind === 'catalog_group';
		const pendingGroupToken = hasExplicitGroupID ? item.groupExternalId : item.upstreamGroup;
		try {
			const bootstrap = await runAfterCanonicalGroupToken(pendingGroupToken, async (acceptedGroupToken) => {
				const [supplierResult, targetResult, credentialResult] = await Promise.all([api.listSuppliers(abortController.signal), api.listTargets(abortController.signal), api.listCredentials(abortController.signal)]);
				return { acceptedGroupToken, supplierResult, targetResult, credentialResult };
			});
			if (bootstrap.status !== 'resolved') {
				setError(sourceIdentityError('上游分组', item.upstreamGroup, item.sourceKind, bootstrap.status, bootstrap.matchCount));
				return;
			}
			const { acceptedGroupToken, supplierResult, targetResult, credentialResult } = bootstrap.value;
			if (requestGeneration !== wizardRequestGeneration.current) return;
			const supplierResolution = resolveSupplierIdentity(supplierResult.data, hasExplicitSupplierID
				? { supplierId: item.supplierId, requireExact: true }
				: { supplierName: item.supplier });
			if (supplierResolution.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', item.supplier, item.sourceKind, supplierResolution.status, supplierResolution.matchCount));
			}
			const supplier = supplierResolution.value;
			const sourceResult = await runAfterCanonicalSupplierID(supplier.id, (canonicalID) => api.supplierSnapshot(canonicalID, abortController.signal));
			if (sourceResult.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', item.supplier, item.sourceKind, sourceResult.status, sourceResult.matchCount));
			}
			const source = sourceResult.value;
			if (requestGeneration !== wizardRequestGeneration.current) return;
			const ownership = verifyCatalogSnapshotOwnership(source.data, supplier.id);
			if (ownership.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游站点', supplier.name, item.sourceKind, ownership.status, ownership.matchCount));
			}
			if (item.sourceKind === 'catalog_group' && source.data.evidence?.response_hash !== item.sourceSnapshotHash) {
				throw new Error('上游目录在选择后已经变化，请重新打开完整目录并选择一次。');
			}
			if (item.sourceKind === 'catalog_group' && !isCatalogEvidenceFresh(source.data)) {
				throw new Error('上游目录证据已经过期，请重新打开完整目录并点击“采集并刷新”。');
			}
			const groupResolution = resolveCatalogGroupIdentity(source.data.groups, hasExplicitGroupID
				? { groupExternalId: acceptedGroupToken, requireExact: true }
				: { groupName: acceptedGroupToken });
			if (groupResolution.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游分组', item.upstreamGroup, item.sourceKind, groupResolution.status, groupResolution.matchCount));
			}
			const group = groupResolution.value;
			const effectiveRatio = group.effective_ratio ?? group.ratio ?? group.raw_ratio;
			if (item.sourceKind === 'catalog_group' && (effectiveRatio === undefined || String(effectiveRatio).trim() === '')) {
				throw new Error('这个分组缺少可核对的真实倍率，暂时不能生成上架预览。');
			}
			const identityResolution = item.sourceKind === 'catalog_group'
				? normalizeResolvedListingSource({
					sourceKind: 'catalog_group', supplier, group,
					evidence: {
						sourceSnapshotHash: item.sourceSnapshotHash,
						sourceEffectiveRatio: String(effectiveRatio),
						sourceObservedAt: source.data.evidence?.observed_at,
					},
				})
				: normalizeResolvedListingSource({
					sourceKind: 'group', supplier, group,
					evidence: {
						sourceSnapshotHash: source.data.evidence?.response_hash,
						sourceEffectiveRatio: effectiveRatio === undefined ? undefined : String(effectiveRatio),
						sourceObservedAt: source.data.evidence?.observed_at,
					},
				});
			if (identityResolution.status !== 'resolved') {
				throw new Error(sourceIdentityError('上游分组', item.upstreamGroup, item.sourceKind, identityResolution.status, identityResolution.matchCount));
			}
			const resolvedIdentity = identityResolution.value;
			const parsedRatio = effectiveRatio === undefined ? null : Number(effectiveRatio);
			const resolvedSelection: ResolvedListingSource = item.sourceKind === 'catalog_group' ? {
				...item,
				supplierId: resolvedIdentity.supplierId,
				groupExternalId: resolvedIdentity.groupExternalId,
				supplier: resolvedIdentity.supplierName,
				upstreamGroup: resolvedIdentity.groupName,
				upstreamRate: parsedRatio !== null && Number.isFinite(parsedRatio) ? parsedRatio : null,
				sourceEffectiveRatio: String(effectiveRatio),
				sourceObservedAt: source.data.evidence?.observed_at,
				resolvedIdentity,
				resolvedSnapshot: source.data,
			} : {
				...item,
				supplierId: resolvedIdentity.supplierId,
				groupExternalId: resolvedIdentity.groupExternalId,
				supplier: resolvedIdentity.supplierName,
				upstreamGroup: resolvedIdentity.groupName,
				resolvedIdentity,
				resolvedSnapshot: source.data,
			};
			setSelected(resolvedSelection);
			setPendingSelection(null);
			setSelectedModels([...resolvedIdentity.modelIds]);
			setChannelName(`${resolvedIdentity.supplierName} · ${resolvedIdentity.groupName}`);
			const apiBaseURL = supplier.metadata?.api_base_url?.trim() || supplier.base_url;
			setChannelBaseUrl(apiBaseURL);
			setCredentials(credentialResult.data);
			const existingKeys = credentialResult.data.filter((credential) => {
				const kind = credential.kind.toLowerCase();
				return kind.includes('api') && !['login', 'browser', 'cookie', 'session'].some((part) => kind.includes(part)) &&
					(!credential.status || credential.status === 'active') && credential.id !== supplier.credential_id && sameOrigin(credential.site_url, apiBaseURL);
			});
			setCredentialMode(existingKeys.length > 0 ? 'existing' : 'dedicated');
			setChannelCredentialId(existingKeys[0]?.id ?? null);
			const enabledTargets = targetResult.data.filter((target) => target.enabled);
			setTargets(enabledTargets);
			const selectedTarget = preferredEnabledTarget(enabledTargets, opportunityTargetId);
			setTargetId(selectedTarget ? String(selectedTarget.id) : '');
			if (selectedTarget) {
				const targetGeneration = ++targetRequestGeneration.current;
				const target = await api.targetSnapshot(selectedTarget.id, abortController.signal);
				if (requestGeneration !== wizardRequestGeneration.current || targetGeneration !== targetRequestGeneration.current) return;
				setTargetGroups(Object.keys(target.data.group_ratio));
				setTargetReadable(true);
			}
		} catch (cause) {
			if (requestGeneration === wizardRequestGeneration.current) setError(getErrorMessage(cause));
		} finally {
			if (requestGeneration === wizardRequestGeneration.current) setLoading(false);
			if (wizardAbortController.current === abortController) wizardAbortController.current = null;
		}
	};

	const selectCatalogGroup = (group: CatalogGroup) => {
		const supplierResolution = resolveSupplierIdentity(catalogSuppliers, { supplierId: catalogSupplierId, requireExact: true });
		const sourceHash = catalogSnapshot?.evidence?.response_hash?.trim();
		if (supplierResolution.status !== 'resolved') {
			setCatalogError(sourceIdentityError('上游站点', catalogSupplierId, 'catalog_group', supplierResolution.status, supplierResolution.matchCount));
			return;
		}
		const supplier = supplierResolution.value;
		if (!catalogSnapshot || !sourceHash) {
			setCatalogError('当前目录证据不完整，请重新选择上游或重新采集。');
			return;
		}
		const ownership = verifyCatalogSnapshotOwnership(catalogSnapshot, supplier.id);
		if (ownership.status !== 'resolved') {
			setCatalogError(sourceIdentityError('上游站点', supplier.name, 'catalog_group', ownership.status, ownership.matchCount));
			return;
		}
		const groupResolution = resolveCatalogGroupIdentity(catalogSnapshot.groups, {
			groupExternalId: group.external_id,
			groupName: group.name,
			requireExact: true,
		});
		if (groupResolution.status !== 'resolved') {
			setCatalogError(sourceIdentityError('上游分组', group.name || group.external_id, 'catalog_group', groupResolution.status, groupResolution.matchCount));
			return;
		}
		const resolvedGroup = groupResolution.value;
		if (!isCatalogEvidenceFresh(catalogSnapshot)) {
			setCatalogError('最近目录证据已经过期，请先点击“采集并刷新”。');
			return;
		}
		const effectiveRatio = resolvedGroup.effective_ratio ?? resolvedGroup.ratio ?? resolvedGroup.raw_ratio;
		if (effectiveRatio === undefined || String(effectiveRatio).trim() === '') {
			setCatalogError('这个分组缺少可核对的真实倍率，暂时不能生成上架预览。');
			return;
		}
		const parsedRatio = effectiveRatio === undefined ? null : Number(effectiveRatio);
		const source: ListingSourceSelection = {
			id: `catalog:${supplier.id}:${resolvedGroup.external_id}`,
			supplier: supplier.name,
			upstreamGroup: resolvedGroup.name || resolvedGroup.external_id,
			upstreamRate: parsedRatio !== null && Number.isFinite(parsedRatio) ? parsedRatio : null,
			modelCount: resolvedGroup.model_ids.length,
			featuredModels: resolvedGroup.model_ids.slice(0, 4),
			foundAt: catalogSnapshot.evidence?.observed_at || supplier.last_success_at || '',
			recommendation: '管理员从当前完整上游目录主动选择',
			status: 'new',
			sourceKind: 'catalog_group',
			supplierId: supplier.id,
			groupExternalId: resolvedGroup.external_id,
			sourceSnapshotHash: sourceHash,
			sourceObservedAt: catalogSnapshot.evidence?.observed_at,
			sourceEffectiveRatio: String(effectiveRatio),
		};
		catalogRequestGeneration.current += 1;
		setCatalogOpen(false);
		void openWizard(source);
	};

	const changeTarget = async (id: string) => {
		targetAbortController.current?.abort();
		const abortController = new AbortController();
		targetAbortController.current = abortController;
		const requestGeneration = ++targetRequestGeneration.current;
		const wizardGeneration = wizardRequestGeneration.current;
		setTargetId(id); setTargetGroups([]); setTargetReadable(false); setError(''); invalidatePreview();
		if (!id) {
			abortController.abort();
			if (targetAbortController.current === abortController) targetAbortController.current = null;
			return;
		}
		try {
			const result = await api.targetSnapshot(id, abortController.signal);
			if (requestGeneration !== targetRequestGeneration.current || wizardGeneration !== wizardRequestGeneration.current) return;
			setTargetGroups(Object.keys(result.data.group_ratio));
			setTargetReadable(true);
		} catch (cause) {
			if (requestGeneration === targetRequestGeneration.current && wizardGeneration === wizardRequestGeneration.current) setError(getErrorMessage(cause));
		} finally {
			if (targetAbortController.current === abortController) targetAbortController.current = null;
		}
	};

	const runPreview = async () => {
		if (!selected || !sourceSupplier || !sourceGroup || !targetId) return;
		previewAbortController.current?.abort();
		const abortController = new AbortController();
		previewAbortController.current = abortController;
		const requestGeneration = ++previewRequestGeneration.current;
		const wizardGeneration = wizardRequestGeneration.current;
		setPreviewing(true); setError(''); setChangeSet(null);
		try {
			if (isBaseline(selected)) throw new Error('首次目录基线必须通过“从完整上游目录上架”入口明确选择。');
			if (selected.sourceKind === 'catalog_group' && !selected.sourceEffectiveRatio?.trim()) throw new Error('当前选择缺少已锁定的真实倍率，请重新打开完整目录。');
			if (!targetReadable) throw new Error('目标站点尚未通过只读检查。');
			if (targetMode === 'existing' && !targetGroups.includes(targetGroup)) throw new Error('请选择当前目标站点实时读回的已有分组。');
			if (selectedModels.length === 0) throw new Error('至少需要保留一个模型。');
			if (!channelBaseUrl.trim()) throw new Error('请填写上游 API Base URL。');
			const parsedChannelType = Number(channelType);
			if (!Number.isInteger(parsedChannelType) || parsedChannelType <= 0) throw new Error('渠道类型必须是正整数。');
			let credentialId = channelCredentialId ?? 0;
			if (credentialMode === 'dedicated') {
				if (!credentialId) {
					if (!dedicatedKey.trim()) throw new Error('请输入独立上游 API Key。');
					const credential = await api.createCredential({
						name: `${channelName.trim()} · 独立上游密钥`, kind: 'api-key', site_url: channelBaseUrl.trim(),
						payload: { authorization: dedicatedKey.trim(), authorization_mode: 'bearer' },
					});
					if (requestGeneration !== previewRequestGeneration.current || wizardGeneration !== wizardRequestGeneration.current) return;
					credentialId = credential.data.id;
					setChannelCredentialId(credentialId);
					setCredentials((current) => [...current, credential.data]);
					setDedicatedKey('');
				}
			}
			if (!credentialId) throw new Error('尚未选择渠道调用 API Key。后台登录凭据只能采集目录和余额，不能用于 NewAPI 转发。');
			const previewIdentity = projectListingPreviewIdentity(selected.resolvedIdentity);
			const result = await api.previewListing({
				...previewIdentity, discovery_kind: selected.sourceKind,
				source_snapshot_hash: selected.sourceKind === 'catalog_group' ? selected.sourceSnapshotHash : undefined,
				expected_source_ratio: selected.sourceKind === 'catalog_group' ? selected.sourceEffectiveRatio : undefined,
				confirm_catalog_group: selected.sourceKind === 'catalog_group' ? true : undefined,
				target_site_id: Number(targetId), target_group_mode: targetMode, target_group: targetGroup.trim(),
				upstream_credential_id: credentialId,
				channel: {
					name: channelName.trim(), type: parsedChannelType, base_url: channelBaseUrl.trim(), models: selectedModels,
					test_model: '', priority: 0, weight: 0, auto_ban: autoBan,
				},
				pricing_rule: {
					create_group_mirror: createGroupMirror,
					mirror_execution_mode: mirrorExecutionMode,
					create_pricing_mapping: createPricingMapping,
					price_anchor: 'source_base',
					group_ratio_rule_type: pricingMode === 'keep' ? 'same' : pricingMode,
					group_ratio_rule_value: pricingMode === 'keep' ? '' : pricingValue.trim(),
					model_price_rule_type: 'same',
					model_price_rule_value: '',
					rule_type: pricingMode === 'keep' ? 'multiply' : pricingMode,
					value: pricingMode === 'keep' ? '1' : pricingValue.trim(), execution_mode: pricingExecutionMode, profit_basis: 'contribution',
					revenue_cash_factor: '1', cost_cash_factor: '1', risk_buffer_rate: '0',
					allow_negative_profit: allowNegativeProfit, subsidy_budget_daily: subsidyBudgetDaily.trim() || '0',
					minimum_ratio: '0', maximum_ratio: '0', rounding_step: recommendedRule && pricingMode === recommendedRule.type && pricingValue.trim() === recommendedRule.value ? recommendedRule.rounding : '0.001',
					model_price_rounding_step: '0.000001',
					sync_group_ratio: false,
					sync_model_prices: true,
					sync_billing_mode: true,
					sync_model_additions: true,
					sync_model_removals: true,
					model_scope_mode: 'mirror_group',
					empty_group_action: 'remove_managed_models',
				},
				idempotency_key: previewKey,
			}, abortController.signal);
			if (requestGeneration !== previewRequestGeneration.current || wizardGeneration !== wizardRequestGeneration.current) return;
			if (result.data.pricing_rule.create_pricing_mapping === false && createPricingMapping) {
				setCreatePricingMapping(false);
			}
			setPreviewPlan(result.data);
			setApplyKey(newIdempotencyKey(`listing-apply-${result.data.id}`));
			const history = await api.listListingChangeSets(result.data.id);
			if (requestGeneration !== previewRequestGeneration.current || wizardGeneration !== wizardRequestGeneration.current) return;
			setChangeHistory(history.data);
		} catch (cause) {
			if (requestGeneration === previewRequestGeneration.current && wizardGeneration === wizardRequestGeneration.current) setError(getErrorMessage(cause));
		} finally {
			if (requestGeneration === previewRequestGeneration.current && wizardGeneration === wizardRequestGeneration.current) setPreviewing(false);
			if (previewAbortController.current === abortController) previewAbortController.current = null;
		}
	};

	const applyPlan = async () => {
		if (!previewPlan) return;
		const requestGeneration = ++applyRequestGeneration.current;
		const wizardGeneration = wizardRequestGeneration.current;
		const frozenPlan = previewPlan;
		setApplying(true); setError('');
		try {
			const current = await api.getListingPlan(frozenPlan.id);
			if (requestGeneration !== applyRequestGeneration.current || wizardGeneration !== wizardRequestGeneration.current) return;
			if (!['ready', 'failed', 'stale'].includes(current.data.status)) throw new Error(`当前预览状态为 ${current.data.status}，不能重复执行。`);
			const result = await api.applyListing(frozenPlan.id, { idempotency_key: applyKey, confirm_enable: true });
			if (requestGeneration !== applyRequestGeneration.current || wizardGeneration !== wizardRequestGeneration.current) return;
			setChangeSet(result.data);
			const history = await api.listListingChangeSets(frozenPlan.id);
			if (requestGeneration !== applyRequestGeneration.current || wizardGeneration !== wizardRequestGeneration.current) return;
			setChangeHistory(history.data);
			await loadListingHistory();
		} catch (cause) {
			if (requestGeneration === applyRequestGeneration.current && wizardGeneration === wizardRequestGeneration.current) {
				setApplyKey(newIdempotencyKey(`listing-apply-${frozenPlan.id}`));
				setError(getErrorMessage(cause));
			}
		} finally {
			if (requestGeneration === applyRequestGeneration.current && wizardGeneration === wizardRequestGeneration.current) setApplying(false);
		}
	};

	const openListingRollback = (item: ListingChangeSet) => {
		setRollbackCandidate(item);
		setRollbackConfirmed(false);
		setRollbackError('');
	};

	const rollbackListing = async () => {
		if (!rollbackCandidate || !rollbackConfirmed) return;
		setRollingBackId(rollbackCandidate.id);
		setRollbackError('');
		try {
			const result = await api.rollbackListingChangeSet(rollbackCandidate.id);
			setRollbackCandidate(result.data);
			if (changeSet?.id === result.data.id) setChangeSet(result.data);
			await loadListingHistory();
			if (result.data.status === 'rolled_back') {
				setRollbackCandidate(null);
				setRollbackConfirmed(false);
			}
		} catch (cause) {
			setRollbackError(getErrorMessage(cause));
			await loadListingHistory();
		} finally {
			setRollingBackId(null);
		}
	};

  return (
    <>
	      <PageHeader eyebrow="新品机会" title="基于本站真实数据的收益建议" description="系统按需求、采购成本、本站售价、可用性与证据置信度计算。默认只展示保守收益下界为正的候选，上架仍需人工确认。" actions={<><Button variant="secondary" onClick={onOpenMirror}><Workflow size={16} />同步已有渠道</Button><Button variant="secondary" onClick={() => void openCatalogPicker()} disabled={catalogLoading}><PackagePlus size={16} />从完整上游目录上架</Button><Button variant="secondary" onClick={() => void loadOpportunities(true)} disabled={opportunityRefreshing}><RefreshCw size={16} className={opportunityRefreshing ? 'spin' : ''} />{opportunityRefreshing ? '正在刷新证据' : '刷新收益证据'}</Button></>} />
		<div className="opportunity-summary">
			<div className="is-recommended"><Sparkles size={18} /><span>建议上架</span><strong>{opportunities?.summary.recommended ?? 0}</strong></div>
			<div><TrendingUp size={18} /><span>持续观察</span><strong>{opportunities?.summary.watch ?? 0}</strong></div>
			<div><PackageCheck size={18} /><span>暂无优势</span><strong>{opportunities?.summary.no_advantage ?? 0}</strong></div>
			<div><AlertTriangle size={18} /><span>数据不足</span><strong>{opportunities?.summary.insufficient_data ?? 0}</strong></div>
			<div className="opportunity-evidence"><span>{opportunities?.has_observed_usage ? '已使用本站 7/30 天模型用量' : '当前使用低置信需求先验'}</span><small>{opportunities?.target ? `目标目录：${opportunities.target.name} · ${opportunities.target.source === 'live' ? '实时读取' : opportunities.target.source === 'cache' ? '可信缓存' : opportunities.target.source === 'stale_cache' ? '过期缓存' : '待刷新'}` : '目标目录待获取'}</small><small>本次计算：{opportunityTime(opportunities?.generated_at)} · 每分钟自动核对</small></div>
		</div>
		{(opportunities?.warnings.length ?? 0) > 0 && <div className="opportunity-warning"><AlertTriangle size={17} /><span>{opportunities?.warnings.join('；')}</span></div>}
		{opportunityError && <div className="form-error">收益建议暂时无法读取：{opportunityError}。原有发现与人工上架流程仍然保留。</div>}
      <div className="toolbar opportunity-toolbar">
			<div className="segmented opportunity-tabs">
				{([['recommended', '建议上架'], ['watch', '持续观察'], ['no_advantage', '暂无优势'], ['insufficient_data', '数据不足'], ['all', '全部']] as const).map(([value, label]) => <button key={value} className={classificationFilter === value ? 'active' : ''} onClick={() => setClassificationFilter(value)}>{label}{value !== 'all' && <b>{opportunities?.summary[value] ?? 0}</b>}</button>)}
			</div>
			<div className="opportunity-filters">
				<label className="opportunity-target-selector"><span>收益目标站点</span><select value={opportunityTargetId} onChange={(event) => { setOpportunityTargetId(event.target.value); void loadOpportunities(false, event.target.value); }} disabled={opportunityTargets.length === 0}><option value="">自动选择</option>{opportunityTargets.map((target) => <option key={target.id} value={target.id}>{target.name}</option>)}</select></label>
				<label><span>模型族</span><select value={familyFilter} onChange={(event) => setFamilyFilter(event.target.value)}><option value="all">全部模型族</option>{families.map((family) => <option key={family} value={family}>{family}</option>)}</select></label>
				<label><span>上游</span><select value={supplierFilter} onChange={(event) => setSupplierFilter(event.target.value)}><option value="all">全部上游</option>{opportunitySuppliers.map((supplier) => <option key={supplier} value={supplier}>{supplier}</option>)}</select></label>
				<label><span>最低日利润</span><input inputMode="decimal" value={minimumProfit} onChange={(event) => setMinimumProfit(event.target.value)} placeholder="不限" /></label>
			</div>
      </div>
		{opportunityLoading ? <div className="inline-loading opportunity-loading"><LoaderCircle className="spin" size={20} />正在核对目录、价格和需求证据…</div> : opportunities && filteredOpportunities.length === 0 ? <EmptyState icon={<PackageCheck size={25} />} title={classificationFilter === 'recommended' ? '当前没有通过保守门槛的建议' : '当前筛选条件下没有候选'} description={classificationFilter === 'recommended' ? '可以查看“持续观察”和“数据不足”，补齐真实用量或目录证据后系统会自动重算。' : '调整模型族、上游或最低收益筛选即可查看其他候选。'} /> : opportunities ? (
			<div className="opportunity-list">
				{filteredOpportunities.map((item) => {
					const discovery = discoveryForOpportunity(item);
					const topContributionModels = [...item.models]
						.sort((left, right) => Math.abs(Number(right.expected_daily_profit) || 0) - Math.abs(Number(left.expected_daily_profit) || 0))
						.slice(0, 3);
					return <article className={`opportunity-row opportunity-${item.classification}`} key={item.id}>
						<div className="opportunity-title"><span className="discovery-icon"><PackagePlus size={20} /></span><div><div><strong>{item.group}</strong><span className={`opportunity-class class-${item.classification}`}>{opportunityLabels[item.classification]}</span>{item.uses_prior && <span className="simple-tag">低置信估算</span>}</div><p>{item.supplier} · {item.model_ids.length} 个模型 · 证据更新于 {item.observed_at || '待采样'}</p><div className="model-chips">{item.model_ids.slice(0, 4).map((model) => <span key={model}>{model}</span>)}{item.model_ids.length > 4 && <span>+{item.model_ids.length - 4}</span>}</div></div></div>
						<div className="opportunity-economics"><div><OpportunityMetricLabel label="预计日利润" help="按推荐规则、日均需求、可用性和证据权重估算，不等于已入账现金。" /><strong>{opportunityMoney(item.expected_daily_profit, item.currency)}</strong></div><div><OpportunityMetricLabel label="预计月利润" help="预计日利润按 30 天外推；需求、价格或可用性变化后会自动重算。" /><strong>{opportunityMoney(item.expected_monthly_profit, item.currency)}</strong></div><div><OpportunityMetricLabel label="预计月节省" help="相对本站当前已启用同模型路线的预计采购成本差额，按 30 天外推。" /><strong>{opportunityMoney(item.expected_monthly_saving, item.currency)}</strong></div><div><OpportunityMetricLabel label="贡献毛利率" help="风险调整后的预计贡献利润除以预计销售收入。" /><strong>{opportunityPercent(item.gross_margin)}</strong></div></div>
						<div className="opportunity-proof"><div><OpportunityMetricLabel label="保守收益下界" help="提高成本上界，同时降低需求、可用性和证据权重后的保守情景。" /><strong>{opportunityMoney(item.conservative_lower_bound, item.currency)}</strong></div><div><OpportunityMetricLabel label="模型覆盖" help="成功比较的价格维度数占目标价格维度总数的比例。" /><strong>{opportunityPercent(item.model_coverage)}</strong></div><div><OpportunityMetricLabel label="可用性" help="基于上游最近状态和目录证据估算的可服务比例。" /><strong>{opportunityPercent(item.availability)}</strong></div><div><OpportunityMetricLabel label="置信度" help="目录证据与本站需求证据合并后的加权可靠度，不是成功概率。" /><strong>{opportunityPercent(item.confidence)}</strong></div></div>
						<div className="opportunity-reason"><span>计算结论</span><p>{item.reason}</p>{item.recommended_rule_type && item.recommended_rule_value && <small className="opportunity-rule">预测对应规则：{pricingRuleLabels[item.recommended_rule_type]} {item.recommended_rule_value}{item.recommended_rounding_step ? `，舍入 ${item.recommended_rounding_step}` : ''}。只有按这套公式和参数上架时，以上利润预测才直接适用。</small>}{item.recommendation_note && <small>{item.recommendation_note}</small>}{item.subsidy_within_budget && <small>公益补贴预算可覆盖预计缺口</small>}{item.models.some((model) => model.demand_unit && model.price_unit) && <small>价格口径：{Array.from(new Set(item.models.filter((model) => model.demand_unit && model.price_unit).map((model) => `${opportunityUnitLabel(model.demand_unit)} / ${opportunityUnitLabel(model.price_unit)}`))).join('，')}</small>}{item.warnings.length > 0 && <small>{item.warnings.join('；')}</small>}{item.hard_filter_reasons.length > 0 && <small>{item.hard_filter_reasons.join('；')}</small>}{topContributionModels.length > 0 && <details className="opportunity-calculation"><summary><CircleHelp size={12} />查看计算依据</summary><div>{topContributionModels.map((model) => <div key={model.model_id}><span><strong>{model.model_id}</strong><small>{opportunityQuantity(model.daily_demand, model.demand_unit)} · {opportunityEvidenceLabel(model.demand_evidence)}</small></span><b>{opportunityMoney(model.expected_daily_profit, item.currency)} / 日</b></div>)}</div>{item.models.length > topContributionModels.length && <small>显示利润贡献最高的 {topContributionModels.length} 个模型，共 {item.models.length} 个模型参与当前计算。</small>}</details>}</div>
						<div className="discovery-actions"><Button onClick={() => void openWizard(discovery, item)} disabled={discovery.status === 'listed'}><Eye size={16} />{item.eligible ? '配置上架' : '查看目录'}</Button></div>
					</article>;
				})}
			</div>
		) : discoveries.length === 0 ? <EmptyState icon={<PackageCheck size={25} />} title="没有待处理的新分组" description="上游目录没有未确认变化，系统会按采集计划继续检查。" /> : (
			<div className="discovery-list">{discoveries.map((item) => <article className="discovery-row" key={item.id}><span className="discovery-icon"><PackagePlus size={21} /></span><div className="discovery-main"><div><strong>{item.upstreamGroup}</strong><span className="simple-tag tag-new">确认新增</span></div><p>{item.supplier} · 首次记录于 {item.foundAt}</p></div><div className="recommendation"><span>发现说明</span><p>{item.recommendation}</p></div><div className="discovery-actions"><Button onClick={() => void openWizard(discoveryListingSource(item))}><Eye size={16} />配置上架</Button></div></article>)}</div>
		)}
		<Section title="一键上架执行记录" description="持续保留渠道、规则、分组倍率和逐模型合同的执行回执；已上架对象可在这里条件回滚。" action={<Button variant="secondary" onClick={() => void loadListingHistory()} disabled={listingHistoryLoading}><RefreshCw size={15} className={listingHistoryLoading ? 'spin' : ''} />刷新记录</Button>}>
			{listingHistoryError && <div className="form-error">执行记录读取失败：{listingHistoryError}</div>}
			{listingHistoryLoading && recentListingChanges.length === 0 ? <div className="inline-loading"><LoaderCircle size={18} className="spin" />正在读取上架执行证据…</div> : recentListingChanges.length === 0 ? <EmptyState icon={<ArchiveRestore size={22} />} title="尚无一键上架执行记录" description="完成一次上架后，渠道、规则、价格合同和安全回滚入口会显示在这里。" /> : <div className="listing-history-list">
				{recentListingChanges.map((item) => {
					const plan = recentListingPlans[item.plan_id];
					const target = opportunityTargets.find((candidate) => candidate.id === item.target_site_id);
					const canRollback = item.status === 'applied' || item.status === 'rolling_back';
					return <article className="listing-history-row" key={item.id}>
						<div className="listing-history-title"><StatusBadge status={listingExecutionHealth(item.status)} label={listingExecutionStatus(item.status)} /><span><strong>{plan?.channel.name || `上架计划 #${item.plan_id}`}</strong><small>{plan ? `${plan.source_group} → ${plan.target_group}` : `目标站点 #${item.target_site_id}`} · 更新于 {formatDateTime(item.updated_at)}</small></span></div>
						<div className="listing-history-evidence"><span><b>目标</b>{target?.name || `#${item.target_site_id}`}</span><span><b>渠道</b>{item.created_channel_id ? `#${item.created_channel_id}` : '未创建'}</span><span><b>规则</b>{item.pricing_policy_id ? `#${item.pricing_policy_id}` : item.status === 'rolled_back' ? '已恢复/移除' : '未绑定'}</span><span><b>合同</b><code title={item.group_model_pricing?.applied_revision}>{shortContractRevision(item.group_model_pricing?.applied_revision)}</code></span></div>
						{plan?.group_model_pricing?.minimum_margin_rate && <small className="listing-history-margin">冻结最低毛利 {listingPercent(plan.group_model_pricing.minimum_margin_rate)} · GroupRatio {plan.proposed_group_ratio}</small>}
						{item.rollback_status && <small className="listing-history-receipt">{item.rollback_status}</small>}
						{item.error && <small className="listing-history-error">{item.error}</small>}
						<div className="listing-history-actions">{canRollback ? <Button variant={item.status === 'rolling_back' ? 'danger' : 'secondary'} onClick={() => openListingRollback(item)} disabled={rollingBackId !== null}>{item.status === 'rolling_back' ? <RefreshCw size={15} /> : <ArchiveRestore size={15} />}{item.status === 'rolling_back' ? '继续安全回滚' : '条件回滚'}</Button> : <span>变更集 #{item.id} · {item.step}</span>}</div>
					</article>;
				})}
			</div>}
		</Section>
	  <Modal open={catalogOpen} onClose={closeCatalogPicker} title="从完整上游目录上架" description="这里展示最近一次成功保存的全部分组，包括首次接入时已经存在的存量分组；只有证据仍新鲜时才可选择，之后仍需生成冻结预览并再次确认执行。" footer={<Button variant="secondary" onClick={closeCatalogPicker}>关闭</Button>}>
		<div className="catalog-listing-picker">
			<div className="catalog-listing-toolbar">
				<div className="catalog-listing-supplier-control"><label className="field"><span>上游站点</span><select value={catalogSupplierId} onChange={(event) => void loadCatalogSupplier(event.target.value)} disabled={catalogLoading || catalogSuppliers.length === 0}><option value="">选择上游</option>{catalogSuppliers.map((supplier, index) => <option key={`${supplier.id}:${index}`} value={supplier.id}>{supplier.name} · #{supplier.id}</option>)}</select></label><Button variant="secondary" onClick={() => void refreshCatalogSupplier()} disabled={catalogLoading || !catalogSupplierId} title="立即向这个上游采集一次并刷新目录证据"><RefreshCw size={15} className={catalogLoading ? 'spin' : ''} />采集并刷新</Button></div>
				<label className="catalog-listing-search"><Search size={16} /><input value={catalogQuery} onChange={(event) => setCatalogQuery(event.target.value)} placeholder="搜索分组或模型" aria-label="搜索完整上游目录" disabled={!catalogSnapshot} /></label>
			</div>
			{catalogSnapshot && <div className="catalog-listing-evidence"><span><b>{catalogSnapshot.groups.length}</b> 个已保存分组</span><span><b>{catalogSnapshot.models.length}</b> 个模型</span><span>采样 {formatDateTime(catalogSnapshot.evidence?.observed_at)}</span><span className={catalogEvidenceFresh ? 'catalog-evidence-fresh' : 'catalog-evidence-stale'}>{catalogEvidenceFresh ? `证据新鲜（≤ ${catalogFreshnessMinutes} 分钟）` : `证据已过期（>${catalogFreshnessMinutes} 分钟）`}</span><code title={catalogSnapshot.evidence?.response_hash}>{shortContractRevision(catalogSnapshot.evidence?.response_hash)}</code></div>}
			<div className="info-callout"><CircleHelp size={18} /><div><strong>完整目录不等于系统推荐</strong><span>这个入口只帮你选择和配置存量分组，不会把它加入收益建议或自动上架。默认按 20% 目标毛利开始，最终以逐模型冻结预览为准。</span></div></div>
			{catalogSnapshot && !catalogEvidenceFresh && <div className="form-error"><AlertTriangle size={16} />这份目录不再满足上架新鲜度要求。点击“采集并刷新”，成功读回后才能选择分组，避免按旧价格上架。</div>}
			{catalogError && <div className="form-error">完整目录暂时无法使用：{catalogError}</div>}
			{catalogLoading ? <div className="inline-loading catalog-listing-loading"><LoaderCircle size={18} className="spin" />正在采集或读取最近目录证据…</div> : catalogSnapshot && catalogGroups.length === 0 ? <EmptyState icon={<PackageCheck size={22} />} title="没有匹配的已保存分组" description="调整搜索条件，或者点击“采集并刷新”后确认上游仍然提供该分组。" /> : catalogSnapshot ? <div className="catalog-listing-groups">{catalogGroups.map((group, index) => {
				const ratio = group.effective_ratio ?? group.ratio ?? group.raw_ratio ?? '待获取';
				const groupIdentity = resolveCatalogGroupIdentity(catalogSnapshot.groups, { groupExternalId: group.external_id, requireExact: true });
				const groupIdentityBlocked = groupIdentity.status !== 'resolved';
				const actionLabel = groupIdentityBlocked ? (groupIdentity.status === 'ambiguous' ? '目录标识冲突' : '目录标识异常')
					: !catalogEvidenceFresh ? '先采集刷新'
						: group.model_ids.length === 0 ? '没有可上架模型'
							: ratio === '待获取' ? '缺少真实倍率' : '选择并配置';
				return <article className="catalog-listing-row" key={`${group.external_id}:${index}`}><div className="catalog-listing-title"><span className="discovery-icon"><PackagePlus size={18} /></span><span><strong>{group.name || group.external_id}</strong><small>{group.external_id !== group.name ? group.external_id : '已保存目录分组'}</small></span></div><div className="catalog-listing-facts"><span><b>真实倍率</b><code>{ratio}</code></span><span><b>模型范围</b><strong>{group.model_ids.length} 个</strong></span></div><div className="model-chips">{group.model_ids.slice(0, 4).map((model, modelIndex) => <span key={`${model}:${modelIndex}`}>{model}</span>)}{group.model_ids.length > 4 && <span>+{group.model_ids.length - 4}</span>}</div><Button onClick={() => selectCatalogGroup(group)} disabled={groupIdentityBlocked || !catalogEvidenceFresh || group.model_ids.length === 0 || ratio === '待获取' || !catalogSnapshot.evidence?.response_hash}><Eye size={15} />{actionLabel}</Button></article>;
			})}</div> : null}
		</div>
	  </Modal>
	  <Modal open={!!(pendingSelection || selected)} onClose={closeWizard} title={`${selected?.upstreamGroup ?? pendingSelection?.upstreamGroup ?? ''} · 一键上架`} description="选择目标分组、分组倍率和上游密钥；模型、价格、计费方式及后续增删会自动完整同步。" footer={selected && !loading ? <><Button variant="ghost" onClick={closeWizard} disabled={previewing || applying}>关闭</Button>{step > 0 && !previewing && !applying && <Button variant="secondary" onClick={() => setStep(step - 1)}>上一步</Button>}{step < 4 ? <Button onClick={() => setStep(step + 1)} disabled={(step === 0 && (selectedModels.length === 0 || (isCatalogSource && !selected.sourceEffectiveRatio))) || (step === 1 && (!targetId || !targetGroup.trim() || (targetMode === 'existing' && !targetGroups.includes(targetGroup)))) || (step === 3 && !channelCredentialId && (credentialMode === 'existing' || !dedicatedKey.trim()))}>下一步</Button> : !previewPlan ? <Button onClick={() => void runPreview()} disabled={previewing || isBaseline(selected) || (isCatalogSource && !selected.sourceEffectiveRatio) || (!channelCredentialId && !dedicatedKey.trim())}>{previewing && <LoaderCircle size={16} className="spin" />}{previewing ? '正在检查' : isBaseline(selected) ? '请从完整目录重新选择' : '检查上架结果'}</Button> : !changeSet ? <Button onClick={() => void applyPlan()} disabled={applying}>{applying && <LoaderCircle size={16} className="spin" />}{applying ? '正在上架并同步' : '执行一键上架'}</Button> : <Button onClick={closeWizard}>完成</Button>}</> : undefined}>
		{loading ? <div className="inline-loading"><LoaderCircle className="spin" size={20} />读取来源目录和目标站点…</div> : selected ? <div className="listing-wizard">
			<fieldset className="listing-wizard-controls" disabled={previewing || applying} aria-busy={previewing || applying}>
			<div className="wizard-steps">{['上游分组', '本站分组', '分组倍率', '上游密钥', '确认上架'].map((label, index) => <button key={label} className={step === index ? 'active' : step > index ? 'done' : ''} disabled={previewing || applying || index > step} onClick={() => index <= step && setStep(index)}><span>{step > index ? <Check size={13} /> : index + 1}</span>{label}</button>)}</div>
			{step === 0 && <div className="wizard-panel"><div className="wizard-source"><span>来源上游</span><strong>{selected.supplier}</strong><small>{selected.upstreamGroup} · 真实倍率 {selected.sourceEffectiveRatio ?? selected.upstreamRate ?? '待获取'} · {isCatalogSource ? '完整上游目录' : '新品发现'}</small></div>{isBaseline(selected) && <div className="info-callout"><AlertTriangle size={18} /><div><strong>这是首次目录基线</strong><span>请关闭向导，使用页面顶部“从完整上游目录上架”选择具体分组。</span></div></div>}<div className="info-callout success"><ShieldCheck size={18} /><div><strong>完整同步 {selectedModels.length} 个模型</strong><span>模型基础价格、计费方式、固定时长以及后续新增和下架都会自动跟随这个上游分组。</span></div></div><div className="model-chips">{selectedModels.slice(0, 12).map((model) => <span key={model}>{model}</span>)}{selectedModels.length > 12 && <span>+{selectedModels.length - 12}</span>}</div></div>}
			{step === 1 && <div className="wizard-panel form-grid"><label className="field full"><span>目标 NewAPI 站点</span><select value={targetId} onChange={(event) => void changeTarget(event.target.value)}><option value="">选择目标站点</option>{targets.map((target) => <option key={target.id} value={target.id}>{target.name}</option>)}</select></label><div className="field full"><span>分组方式</span><div className="segmented"><button className={targetMode === 'new' ? 'active' : ''} onClick={() => { setTargetMode('new'); setTargetGroup(selected.upstreamGroup.toLowerCase().replaceAll(' ', '-')); invalidatePreview(); }}>新建分组</button><button className={targetMode === 'existing' ? 'active' : ''} onClick={() => { setTargetMode('existing'); setTargetGroup(''); invalidatePreview(); }}>使用已有分组</button></div></div>{targetMode === 'new' ? <label className="field full"><span>新分组名称</span><input value={targetGroup} onChange={(event) => { setTargetGroup(event.target.value); invalidatePreview(); }} /></label> : <label className="field full"><span>已有分组</span><select value={targetGroup} onChange={(event) => { setTargetGroup(event.target.value); invalidatePreview(); }}><option value="">选择分组</option>{targetGroups.map((group) => <option key={group}>{group}</option>)}</select></label>}</div>}
			{step === 2 && <div className="wizard-panel form-grid">
				<div className="info-callout success full"><Workflow size={18} /><div><strong>两个配套规则各管一件事</strong><span>分组镜像只同步模型、计费方式和逐模型基础价；价格映射只同步分组倍率，互不重复。</span></div></div>
				<div className="listing-rule-option full"><label className="check-row"><input type="checkbox" checked={createGroupMirror} onChange={(event) => { setCreateGroupMirror(event.target.checked); invalidatePreview(); }} /><span><strong>创建并启用分组镜像</strong><small>上游增删模型或修改模型真实价格时，同步到这个本站分组。</small></span></label>{createGroupMirror && <div className="segmented"><button className={mirrorExecutionMode === 'auto' ? 'active' : ''} onClick={() => { setMirrorExecutionMode('auto'); invalidatePreview(); }}>自动同步</button><button className={mirrorExecutionMode === 'approval' ? 'active' : ''} onClick={() => { setMirrorExecutionMode('approval'); invalidatePreview(); }}>手动检查</button></div>}</div>
				<div className="listing-rule-option full"><label className="check-row"><input type="checkbox" checked={createPricingMapping} onChange={(event) => { setCreatePricingMapping(event.target.checked); invalidatePreview(); }} /><span><strong>创建并启用价格映射</strong><small>上游分组倍率变化时，按下面规则更新本站分组倍率。</small></span></label>{createPricingMapping && <div className="segmented"><button className={pricingExecutionMode === 'auto' ? 'active' : ''} onClick={() => { setPricingExecutionMode('auto'); invalidatePreview(); }}>自动执行</button><button className={pricingExecutionMode === 'approval' ? 'active' : ''} onClick={() => { setPricingExecutionMode('approval'); invalidatePreview(); }}>手动批准</button></div>}</div>
				<label className="field"><span>分组倍率规则</span><select value={pricingMode} onChange={(event) => { setPricingMode(event.target.value as PolicyMode | 'keep'); invalidatePreview(); }}><option value="keep">与上游倍率相同</option><option value="target_margin">目标毛利率</option><option value="markup">按成本加成</option><option value="multiply">乘算</option><option value="add">固定加值</option><option value="fixed">固定倍率</option></select></label>
				<label className="field"><span>规则数值</span><input value={pricingMode === 'keep' ? '' : pricingValue} onChange={(event) => { setPricingValue(event.target.value); invalidatePreview(); }} disabled={pricingMode === 'keep'} placeholder={pricingMode === 'keep' ? '无需填写' : '例如 0.2'} /><small>{pricingMode === 'target_margin' ? '0.2 表示目标毛利率 20%。' : pricingMode === 'keep' ? '本站分组倍率直接跟随上游真实倍率。' : '只影响本站分组倍率，不改变逐模型基础价。'}</small></label>
				{recommendedRule && !isUsingRecommendedRule && <Button variant="secondary" onClick={() => { setPricingMode(recommendedRule.type); setPricingValue(recommendedRule.value); invalidatePreview(); }}><RefreshCw size={15} />使用系统建议：{pricingRuleLabels[recommendedRule.type]} {recommendedRule.value}</Button>}
			</div>}
			{step === 3 && <div className="wizard-panel">
				<div className="credential-role-grid"><div className="credential-role-card"><span>后台登录凭据</span><strong>{loginCredential ? `${loginCredential.name} · #${loginCredential.id}` : sourceSupplier?.credential_id ? `凭据 #${sourceSupplier.credential_id}` : '未配置'}</strong><small>只用于读取目录、倍率、余额和用量，不会写入 NewAPI 渠道。</small></div><div className={`credential-role-card ${selectedChannelCredential || channelCredentialId ? 'is-ready' : 'is-missing'}`}><span>渠道调用 API Key</span><strong>{selectedChannelCredential ? `${selectedChannelCredential.name} · #${selectedChannelCredential.id}` : channelCredentialId ? `新凭据 #${channelCredentialId}` : '尚未补齐'}</strong><small>NewAPI 实际转发模型请求时使用；必须与后台登录会话分离。</small></div></div>
				<div className="segmented credential-mode"><button className={credentialMode === 'existing' ? 'active' : ''} disabled={channelCredentials.length === 0} onClick={() => { setCredentialMode('existing'); setChannelCredentialId(channelCredentials[0]?.id ?? null); invalidatePreview(); }}>选择已有 API Key</button><button className={credentialMode === 'dedicated' ? 'active' : ''} onClick={() => { setCredentialMode('dedicated'); setChannelCredentialId(null); invalidatePreview(); }}>录入独立 API Key</button></div>
				{credentialMode === 'existing' ? <label className="field credential-key"><span>同源且可用的渠道 API Key</span><select value={channelCredentialId ?? ''} onChange={(event) => { setChannelCredentialId(Number(event.target.value) || null); invalidatePreview(); }}><option value="">请选择</option>{channelCredentials.map((credential) => <option key={credential.id} value={credential.id}>{credential.name} · #{credential.id}</option>)}</select><small>已排除供应商登录凭据、Cookie、浏览器会话、失效凭据和不同源密钥。</small></label> : <>
					<div className="upstream-key-create"><div><strong>先去上游创建这个分组专用的 Key</strong><span>上游：{selected.supplier} · 分组：{selected.upstreamGroup}。建议名称：RelayOps · {selected.upstreamGroup}</span>{keyManagementLinks && <a className="upstream-key-home" href={keyManagementLinks.homeUrl} target="_blank" rel="noopener noreferrer">打开上游站点首页</a>}</div><div className="upstream-key-actions">{keyManagementLinks?.options.map((option) => <a key={option.url} className="button button-secondary upstream-key-link" href={option.url} target="_blank" rel="noopener noreferrer"><ExternalLink size={15} />{option.label}</a>)}{keyManagementLinks && keyManagementLinks.options.length === 0 && <a className="button button-secondary upstream-key-link" href={keyManagementLinks.homeUrl} target="_blank" rel="noopener noreferrer"><ExternalLink size={15} />打开上游站点</a>}</div></div>
					<label className="field credential-key"><span>本渠道独立上游 API Key</span><input type="password" autoComplete="off" value={dedicatedKey} onChange={(event) => { setDedicatedKey(event.target.value); setChannelCredentialId(null); invalidatePreview(); }} placeholder={channelCredentialId ? `已安全保存为凭据 #${channelCredentialId}` : '创建后粘贴到这里，按 Bearer 方式安全保存'} />{keyManagementLinks && !keyManagementLinks.configured && <small>{sourceSupplier?.adapter.trim().toLowerCase() === 'newapi' ? 'NewAPI 有新版和经典版两种官方入口；请选择能正常打开的一项。' : '该站点尚未配置经过确认的 Key 管理页；如候选入口不适用，可在“上游站点 → 编辑配置 → 高级端点设置”填写实际路径。'}</small>}</label>
					<div className="info-callout"><KeyRound size={18} /><div><strong>预览前进入服务端加密存储</strong><span>页面和预览记录都不会回显密钥；该 Key 仅用于新渠道正常转发。</span></div></div>
				</>}
			</div>}
			{step === 4 && <div className="wizard-panel">
				<div className="listing-checks">
					<span className={sourceSnapshot ? 'passed' : ''}><Check size={16} />来源目录可读取</span>
					<span className={targetReadable ? 'passed' : ''}><Check size={16} />目标配置可读取</span>
					<span className={selectedModels.length > 0 ? 'passed' : ''}><Check size={16} />已选择 {selectedModels.length} 个模型</span>
					<span className="passed"><Check size={16} />{createGroupMirror ? `分组镜像 · ${mirrorExecutionMode === 'auto' ? '自动' : '手动'}` : '不创建分组镜像'}</span>
					<span className="passed"><Check size={16} />{createPricingMapping ? `价格映射 · ${pricingExecutionMode === 'auto' ? '自动' : '手动'}` : '不创建价格映射'}</span>
					<span className={contractPreview?.supported || contractPreview?.exact ? 'passed' : ''}><ShieldCheck size={16} />{contractPreview?.mode === 'atomic_contract' ? '支持逐模型原子合同' : contractPreview?.exact ? '旧站精确回退已证明' : '服务端配置读回验证'}</span>
				</div>
				{!previewPlan && <div className="info-callout"><ShieldCheck size={18} /><div><strong>下一步只生成冻结预览</strong><span>系统会逐模型核对上游现金成本、计费单位、本站基础价、分组倍率和最低真实毛利；此时不会创建渠道或写入价格。</span></div></div>}
				{previewPlan && <>
					<div className="listing-preview-grid">
						<div><span>目标计价能力</span><strong>{contractPreview?.mode === 'atomic_contract' ? '逐模型原子合同' : contractPreview?.exact ? '旧站精确回退' : '不可安全表示'}</strong></div>
						<div><span>价格锚点</span><strong>{contractPreview?.price_anchor === 'source_base' ? '模型同价，分组控利润' : '固定最终售价或毛利'}</strong></div>
						<div><span>分组倍率</span><strong>{previewPlan.proposed_group_ratio}</strong></div>
						<div><span>逐模型价格</span><strong>{contractModels.length || selectedModels.length} 个模型</strong></div>
						{groupVisibilityPreview?.required && <div><span>用户可见性</span><strong>{groupVisibilityPreview.changed ? '自动加入可用分组' : '已在可用分组中'}</strong></div>}
						<div><span>最低真实毛利</span><strong>{contractPreview?.minimum_margin_defined ? listingPercent(contractPreview.minimum_margin_rate) : 'N/A（零成本）'}</strong></div>
					</div>
					{contractPreview?.mode === 'atomic_contract' && <div className="listing-contract-summary">
						<div><span>当前合同</span><code title={contractPreview.expected_revision}>{shortContractRevision(contractPreview.expected_revision)}</code></div>
						<ArrowRight size={16} />
						<div><span>拟写合同</span><code title={contractPreview.desired_revision}>{shortContractRevision(contractPreview.desired_revision)}</code></div>
						<small>模型价格与单位一次原子写入；执行时若 revision 已变化，会停止而不是覆盖人工修改。</small>
					</div>}
					{contractPreview?.legacy_fallback_reason && <div className="info-callout"><CircleHelp size={18} /><div><strong>旧版目标站兼容回退</strong><span>{contractPreview.legacy_fallback_reason}</span></div></div>}
					{contractExcludedModels.length > 0 && <div className="info-callout"><CircleHelp size={18} /><div><strong>已自动排除 {contractExcludedModels.length} 个不兼容模型</strong><span>{contractExcludedModels.map((model) => `${model}（${contractPreview?.exclusion_reasons?.[model] || '本站计费合同不支持'}）`).join('；')}。其余模型继续上架，这些模型同时写入分组镜像例外，后续不会被上架、下架或改价。</span></div></div>}
					{groupVisibilityPreview?.required && <div className="info-callout"><ShieldCheck size={18} /><div><strong>{groupVisibilityPreview.changed ? `新分组 ${previewPlan.target_group} 将自动开放给用户` : `分组 ${previewPlan.target_group} 已对用户开放`}</strong><span>{groupVisibilityPreview.changed ? '系统会在渠道模型和价格配置读回通过后，以并发保护写入用户可用分组；若后续步骤失败，会按冻结哈希条件恢复。' : '本次不会重复修改用户可用分组；执行时仍会核对冻结哈希，发现人工改动就停止。'}</span></div></div>}
					{(contractPreview?.blocking_reasons?.length ?? 0) > 0 && <div className="form-error">{contractPreview?.blocking_reasons?.join('；')}</div>}
					{contractModels.length > 0 && <div className="listing-price-vector">
						<div className="listing-price-vector-heading"><div><strong>逐模型真实价格</strong><span>模型基础价、计费方式和单位跟随上游；充值折算只进入上游现金成本一次，利润由分组倍率控制。</span></div><small>舍入 {contractPreview?.rounding_step || '0.000001'}</small></div>
						<div className="listing-price-vector-table"><table><thead><tr><th>模型</th><th>计费维度</th><th>上游现金成本</th><th>本站基础价</th><th>用户实付</th><th>真实毛利</th></tr></thead><tbody>{contractModels.flatMap((model) => model.dimensions.map((dimension, index) => <tr key={`${model.model}-${dimension.dimension}`}><td>{index === 0 ? <><strong>{model.model}</strong><small>{model.billing_unit}{model.fixed_duration_seconds ? ` · 固定 ${model.fixed_duration_seconds} 秒` : ''}</small></> : null}</td><td>{listingDimensionLabels[dimension.dimension] || dimension.dimension}</td><td>{model.currency} {listingDecimal(dimension.risk_adjusted_cash_cost)}</td><td>{listingDecimal(dimension.target_base_price)}</td><td>{model.currency} {listingDecimal(dimension.effective_sale)}</td><td className={dimension.margin_defined ? (Number(dimension.margin_rate) < 0 ? 'is-negative' : 'is-positive') : undefined}>{dimension.margin_defined ? listingPercent(dimension.margin_rate) : 'N/A'}</td></tr>))}</tbody></table></div>
					</div>}
					<div className="info-callout success"><Check size={18} /><div><strong>只读预览 #{previewPlan.id} 已冻结</strong><span>{previewPlan.profit_verification}。{changeHistory.length > 0 ? `已有 ${changeHistory.length} 条执行记录。` : '尚未执行任何生产写入。'}</span></div></div>
					{previewPlan.calculation.warnings.length > 0 && <div className="form-error">{previewPlan.calculation.warnings.join('；')}</div>}
					{!changeSet && <div className="info-callout success"><Check size={18} /><div><strong>可以一键上架</strong><span>点击“执行一键上架”后，系统会创建渠道、同步价格并读回确认，不会发送产生费用的模型请求。</span></div></div>}
				</>}
				{changeSet && <div className={`listing-result ${changeSet.status === 'applied' && changeSet.channel_enabled ? 'success' : 'failed'}`}><strong>{changeSet.status === 'applied' && changeSet.channel_enabled ? '上架完成并已启用' : `执行状态：${listingExecutionStatus(changeSet.status)}`}</strong><span>步骤：{changeSet.step} · 变更集 #{changeSet.id}{changeSet.created_channel_id ? ` · 渠道 #${changeSet.created_channel_id}` : ''}</span>{changeSet.group_model_pricing?.applied_revision && <small>逐模型合同已读回：{shortContractRevision(changeSet.group_model_pricing.applied_revision)}</small>}{groupVisibilityReceipt?.applied_hash && <small>用户可用分组已读回：{groupVisibilityReceipt.changed ? '已加入本次新分组' : '原状态保持不变'} · {shortContractRevision(groupVisibilityReceipt.applied_hash)}</small>}{changeSet.rollback_status && <small>{changeSet.rollback_status}</small>}{changeSet.error && <small>{changeSet.error}</small>}{(changeSet.status === 'applied' || changeSet.status === 'rolling_back') && <div className="listing-result-actions"><Button variant="secondary" onClick={() => openListingRollback(changeSet)}><ArchiveRestore size={15} />{changeSet.status === 'rolling_back' ? '继续安全回滚' : '条件回滚'}</Button></div>}</div>}
			</div>}
			</fieldset>
			{error && <div className="form-error">{error}</div>}
		</div> : error ? <div className="form-error">{error}</div> : null}
      </Modal>
	  <Modal open={!!rollbackCandidate} onClose={() => { if (rollingBackId === null) { setRollbackCandidate(null); setRollbackConfirmed(false); setRollbackError(''); } }} title="确认一键上架条件回滚" description="服务端会先确认合同 revision、GroupRatio、渠道 ownership 和跟价规则仍与上架回执完全一致；任何预检冲突都不会写目标站点。" footer={<><Button variant="ghost" onClick={() => { setRollbackCandidate(null); setRollbackConfirmed(false); setRollbackError(''); }} disabled={rollingBackId !== null}>取消</Button><Button variant="danger" onClick={() => void rollbackListing()} disabled={!rollbackConfirmed || rollingBackId !== null}>{rollingBackId !== null ? <LoaderCircle size={16} className="spin" /> : <ArchiveRestore size={16} />}{rollingBackId !== null ? '正在停流、回滚并读回' : rollbackCandidate?.status === 'rolling_back' ? '确认继续回滚' : '确认条件回滚'}</Button></>}>
		{rollbackCandidate && <div className="config-confirmation listing-rollback-confirmation">
			<div><span>变更集</span><strong>#{rollbackCandidate.id} · {listingExecutionStatus(rollbackCandidate.status)}</strong></div>
			<div><span>目标与渠道</span><strong>站点 #{rollbackCandidate.target_site_id} · 渠道 #{rollbackCandidate.created_channel_id ?? '未知'}</strong></div>
			<div><span>跟价规则</span><strong>{rollbackCandidate.pricing_policy_id ? `#${rollbackCandidate.pricing_policy_id}` : '已在前序回滚中恢复'}</strong></div>
			<div><span>当前步骤</span><strong>{rollbackCandidate.step}</strong></div>
			<div className="full"><span>必须仍匹配的 GroupRatio 哈希</span><code>{rollbackCandidate.applied_target_hash || '缺少执行后哈希，服务端会拒绝'}</code></div>
			<div className="full"><span>必须仍匹配的逐模型合同 revision</span><code>{rollbackCandidate.group_model_pricing?.applied_revision || '旧版精确回退，无逐模型合同写入'}</code></div>
			{rollbackCandidate.group_model_pricing?.group_visibility?.applied_hash && <div className="full"><span>必须仍匹配的用户可用分组哈希</span><code>{rollbackCandidate.group_model_pricing.group_visibility.applied_hash}</code></div>}
			<div className="full listing-rollback-order"><span>固定安全顺序</span><strong>禁用自有渠道 → 条件恢复用户可见分组 → 条件恢复逐模型合同 → 条件恢复 GroupRatio → 删除自有渠道 → 恢复或移除跟价规则</strong></div>
			{rollbackCandidate.rollback_status && <div className="full"><span>已有回滚回执</span><code>{rollbackCandidate.rollback_status}</code></div>}
			{rollbackError && <div className="full listing-rollback-error"><span>本次未完成</span><strong>{rollbackError}</strong></div>}
			<label className="check-row full"><input type="checkbox" checked={rollbackConfirmed} onChange={(event) => setRollbackConfirmed(event.target.checked)} /><span>我确认回滚这一条上架记录；若服务端发现后续人工或自动修改，应拒绝覆盖并保留现场</span></label>
		</div>}
	  </Modal>
    </>
  );
}

type ProposalActionResult = { ok: boolean; message: string };
type ChangeTab = 'pending' | 'all' | 'completed' | 'rejected' | 'failed' | 'stale';
type ActivityStatusFilter = '' | 'healthy' | 'warning' | 'error' | 'unknown';
type ActivityDeleteConfirmation = { mode: 'selected' | 'filtered'; count: number };

const activityCategoryOptions: Array<{ value: ActivityRecord['kind']; label: string }> = [
	{ value: 'collect', label: '采集' },
	{ value: 'price', label: '价格映射' },
	{ value: 'mirror', label: '分组镜像' },
	{ value: 'listing', label: '一键上架' },
	{ value: 'balance', label: '余额账务' },
	{ value: 'auth', label: '授权续约' },
	{ value: 'system', label: '系统配置' },
];

const activityStatusOptions: Array<{ value: ActivityStatusFilter; label: string }> = [
	{ value: '', label: '全部状态' },
	{ value: 'healthy', label: '成功 / 信息' },
	{ value: 'warning', label: '需关注' },
	{ value: 'error', label: '失败' },
	{ value: 'unknown', label: '其他' },
];

function activityTimeISO(value: string) {
	if (!value) return undefined;
	const parsed = new Date(value);
	return Number.isNaN(parsed.getTime()) ? undefined : parsed.toISOString();
}

function ActivityDiagnostic({ activity }: { activity: ActivityRecord }) {
	const metadata = activity.metadata;
	if (!metadata?.diagnostic_version) return null;
	const upstream = [metadata.supplier_name, metadata.source_group_name || metadata.source_group].filter(Boolean).join(' / ');
	const target = [metadata.target_site_name, metadata.target_group].filter(Boolean).join(' / ');
	return <div className="activity-diagnostic">
		{metadata.policy_id && <span><b>规则</b>#{metadata.policy_id} {metadata.policy_name}</span>}
		{upstream && <span><b>上游</b>{upstream}</span>}
		{target && <span><b>本站</b>{target}</span>}
		<span><b>模型</b>{metadata.matched_model_count || '0'} 个{metadata.matched_model_samples ? ` · ${metadata.matched_model_samples}` : ''}</span>
		{metadata.target_catalog_status && <span><b>目标目录</b>{metadata.target_catalog_status} · {metadata.target_catalog_model_count || '0'} 个模型</span>}
		{metadata.readback_revision && <span><b>合同读回</b><code title={metadata.readback_revision}>{shortContractRevision(metadata.readback_revision)}</code></span>}
		{metadata.changed_models && <span><b>逐模型变更</b>{metadata.changed_models} 个 · 最低毛利 {listingPercent(metadata.minimum_margin)}</span>}
		{metadata.repeat_count && Number(metadata.repeat_count) > 1 && <span><b>重复</b>{metadata.repeat_count} 次已合并</span>}
	</div>;
}

export function ChangesPage({ proposals, activities, onApply, onReject, onRollback }: { proposals: ChangeProposal[]; activities: ActivityRecord[]; onApply: (id: string) => Promise<ProposalActionResult>; onReject: (id: string) => Promise<ProposalActionResult>; onRollback: (id: string) => Promise<ProposalActionResult> }) {
  const [tab, setTab] = useState<ChangeTab>('pending');
  const [expanded, setExpanded] = useState<string | null>(proposals.find((item) => item.status === 'pending')?.id ?? null);
  const [confirming, setConfirming] = useState<ChangeProposal | null>(null);
  const [workingId, setWorkingId] = useState<string | null>(null);
  const [actionResult, setActionResult] = useState<ProposalActionResult | null>(null);
	const [activityItems, setActivityItems] = useState<ActivityRecord[]>(activities);
	const [activityPage, setActivityPage] = useState(1);
	const [activityLimit, setActivityLimit] = useState(30);
	const [activityTotal, setActivityTotal] = useState(activities.length);
	const [activityFrom, setActivityFrom] = useState('');
	const [activityTo, setActivityTo] = useState('');
	const [activityCategory, setActivityCategory] = useState<'' | ActivityRecord['kind']>('');
	const [activityStatus, setActivityStatus] = useState<ActivityStatusFilter>('');
	const [activityExcludedCategory, setActivityExcludedCategory] = useState<'' | ActivityRecord['kind']>('');
	const [activityLoading, setActivityLoading] = useState(false);
	const [activityError, setActivityError] = useState('');
	const [activityNotice, setActivityNotice] = useState('');
	const [activityRefresh, setActivityRefresh] = useState(0);
	const [selectedActivityIDs, setSelectedActivityIDs] = useState<Set<string>>(() => new Set());
	const [activityDeleteConfirmation, setActivityDeleteConfirmation] = useState<ActivityDeleteConfirmation | null>(null);
	const [activityDeleting, setActivityDeleting] = useState(false);
  const completed = (status: ChangeProposal['status']) => ['applied', 'no_change', 'rolled_back'].includes(status);
  const filtered = useMemo(() => proposals.filter((item) => tab === 'all' || (tab === 'completed' ? completed(item.status) : item.status === tab)), [proposals, tab]);
  const statusLabel = (status: ChangeProposal['status']) => ({ pending: '等待确认', applying: '正在应用', applied: '已应用', no_change: '当前已匹配', stale: '证据已变化', rejected: '已拒绝', failed: '应用失败', rolling_back: '正在回滚', rolled_back: '已回滚' })[status];
	const activityFromISO = activityTimeISO(activityFrom);
	const activityToISO = activityTimeISO(activityTo);
	const activityRangeInvalid = !!activityFromISO && !!activityToISO && activityFromISO > activityToISO;
	const hasActivityFilters = !!(activityFromISO || activityToISO || activityCategory || activityStatus || activityExcludedCategory);
	const activityPageCount = Math.max(1, Math.ceil(activityTotal / activityLimit));
	const currentActivityIDs = activityItems.map((item) => item.id);
	const allCurrentActivitiesSelected = currentActivityIDs.length > 0 && currentActivityIDs.every((id) => selectedActivityIDs.has(id));

	useEffect(() => {
		if (activityRangeInvalid) {
			setActivityError('开始时间不能晚于结束时间');
			setActivityLoading(false);
			return;
		}
		let active = true;
		setActivityLoading(true);
		setActivityError('');
		void api.listActivities({
			page: activityPage,
			limit: activityLimit,
			from: activityFromISO,
			to: activityToISO,
			category: activityCategory,
			status: activityStatus,
			exclude_category: activityExcludedCategory,
		}).then((result) => {
			if (!active) return;
			setActivityItems(result.data.items);
			setActivityTotal(result.data.total);
			setSelectedActivityIDs(new Set());
			const finalPage = Math.max(1, Math.ceil(result.data.total / activityLimit));
			if (activityPage > finalPage) setActivityPage(finalPage);
		}).catch((error) => {
			if (active) setActivityError(getErrorMessage(error));
		}).finally(() => {
			if (active) setActivityLoading(false);
		});
		return () => { active = false; };
	}, [activityCategory, activityExcludedCategory, activityFromISO, activityLimit, activityPage, activityRangeInvalid, activityRefresh, activityStatus, activityToISO]);

	const resetActivityPaging = () => {
		setActivityPage(1);
		setSelectedActivityIDs(new Set());
		setActivityNotice('');
	};
	const toggleCurrentActivities = (checked: boolean) => {
		setSelectedActivityIDs((current) => {
			const next = new Set(current);
			for (const id of currentActivityIDs) checked ? next.add(id) : next.delete(id);
			return next;
		});
	};
	const toggleActivity = (id: string, checked: boolean) => {
		setSelectedActivityIDs((current) => {
			const next = new Set(current);
			checked ? next.add(id) : next.delete(id);
			return next;
		});
	};
	const resetActivityFilters = () => {
		setActivityFrom('');
		setActivityTo('');
		setActivityCategory('');
		setActivityStatus('');
		setActivityExcludedCategory('');
		resetActivityPaging();
	};
	const deleteActivities = async () => {
		if (!activityDeleteConfirmation) return;
		setActivityDeleting(true);
		setActivityError('');
		setActivityNotice('');
		try {
			const input = activityDeleteConfirmation.mode === 'selected'
				? { ids: [...selectedActivityIDs].map(Number).filter((id) => Number.isInteger(id) && id > 0), limit: 500 }
				: {
					from: activityFromISO,
					to: activityToISO,
					category: activityCategory,
					status: activityStatus,
					exclude_category: activityExcludedCategory,
					all: !hasActivityFilters,
					limit: 500,
				};
			const result = await api.deleteActivities(input);
			setActivityNotice(`已删除 ${result.data.deleted} 条日志${activityDeleteConfirmation.mode === 'filtered' && activityTotal > result.data.deleted ? '；如需继续清理，可再次执行' : ''}`);
			setActivityDeleteConfirmation(null);
			setSelectedActivityIDs(new Set());
			setActivityRefresh((value) => value + 1);
		} catch (error) {
			setActivityError(getErrorMessage(error));
		} finally {
			setActivityDeleting(false);
		}
	};
	const runAction = async (item: ChangeProposal, action: 'apply' | 'reject' | 'rollback') => {
		setWorkingId(item.id);
		setActionResult(null);
		const result = action === 'apply' ? await onApply(item.id) : action === 'reject' ? await onReject(item.id) : await onRollback(item.id);
		setActionResult(result);
		setWorkingId(null);
		if (action === 'apply') setConfirming(null);
	};

  return (
    <>
      <PageHeader eyebrow="审计与执行" title="变更中心" description="自动化只负责发现和计算。每项生产变更都有差异预览、读回验证与可追溯记录。" />
		{actionResult && <div className={`change-result-banner ${actionResult.ok ? 'success' : 'error'}`} role="status">{actionResult.ok ? <Check size={18} /> : <AlertTriangle size={18} />}<span>{actionResult.message}</span><button aria-label="关闭执行结果" onClick={() => setActionResult(null)}><X size={16} /></button></div>}
      <div className="toolbar">
        <div className="segmented">{([['pending', '等待确认'], ['all', '全部记录'], ['completed', '已完成'], ['stale', '已过期'], ['rejected', '已拒绝'], ['failed', '失败']] as const).map(([value, label]) => <button key={value} className={tab === value ? 'active' : ''} onClick={() => setTab(value)}>{label}{value === 'pending' && <b>{proposals.filter((item) => item.status === 'pending').length}</b>}</button>)}</div>
      </div>
      {filtered.length === 0 ? <EmptyState icon={<Check size={25} />} title="这里已经处理完了" description="当前分类下没有变更记录。" /> : (
        <div className="change-list">
          {filtered.map((item) => {
            const isOpen = expanded === item.id;
			const isContract = item.pricingMode === 'group_model_contract';
			const health = completed(item.status) ? 'healthy' : ['pending', 'stale', 'applying', 'rolling_back'].includes(item.status) ? 'warning' : item.status === 'failed' ? 'error' : 'unknown';
            return (
              <article className={`change-item ${isOpen ? 'expanded' : ''}`} key={item.id}>
                <button className="change-summary" onClick={() => setExpanded(isOpen ? null : item.id)}>
                  <span className={`change-type type-${item.type}`}>{item.type === 'price' ? '价' : item.type === 'listing' ? '新' : item.type === 'credential' ? '密' : '配'}</span>
                  <span className="change-title"><strong>{item.title}</strong><small>{item.target} · {formatDateTime(item.createdAt)}</small></span>
                  <StatusBadge status={health} label={statusLabel(item.status)} />
                  {isOpen ? <ChevronUp size={18} /> : <ChevronDown size={18} />}
                </button>
                {isOpen && <div className="change-detail">
                  <div className="change-reason"><span>触发原因</span><strong>{item.reason}</strong><small>来源：{item.createdBy}</small></div>
				  {isContract && <div className="change-contract-summary"><div><span>计价方式</span><strong>逐模型原子合同</strong></div><div><span>变更模型</span><strong>{item.changedModels ?? item.modelChanges?.length ?? 0} 个</strong></div><div><span>最低真实毛利</span><strong>{listingPercent(item.minimumMargin)}</strong></div><div><span>并发保护</span><strong>revision CAS + 写后读回</strong></div></div>}
                  <div className="diff"><div><span>变更前</span><code>{item.before}</code></div><ArrowRight size={19} /><div><span>变更后</span><code>{item.after}</code></div></div>
				  {isContract && (item.modelChanges?.length ?? 0) > 0 && <div className="change-model-prices"><div className="change-model-prices-heading"><strong>真实模型价格差异</strong><span>旧合同没有独立项时显示“继承旧全局计价”，不会伪造一个旧价格。</span></div><div className="listing-price-vector-table"><table><thead><tr><th>模型</th><th>计费维度</th><th>上游现金成本</th><th>变更前用户价</th><th>变更后用户价</th><th>毛利</th></tr></thead><tbody>{item.modelChanges?.flatMap((model) => model.dimensions.map((dimension, index) => <tr key={`${model.model}-${dimension.dimension}`}><td>{index === 0 ? <><strong>{model.model}</strong><small>{model.billingUnit}</small></> : null}</td><td>{listingDimensionLabels[dimension.dimension] || dimension.dimension}</td><td>{dimension.currency || model.currency} {dimension.sourceCashCost ? listingDecimal(dimension.sourceCashCost) : '未知'}</td><td>{dimension.currentInherited ? '继承旧全局计价' : `${dimension.currency || model.currency} ${listingDecimal(dimension.currentSale)}`}</td><td>{dimension.currency || model.currency} {listingDecimal(dimension.proposedSale)}</td><td className={Number(dimension.marginRate) < 0 ? 'is-negative' : 'is-positive'}>{listingPercent(dimension.marginRate)}</td></tr>))}</tbody></table></div></div>}
                  <div className="impact-note"><AlertTriangle size={18} /><div><strong>影响评估</strong><span>{item.impact}</span></div></div>
				  {item.readback && <div className="change-readback"><Check size={17} /><div><strong>写后读回</strong><span>{isContract ? '目标站合同 revision' : '目标站实际值'}：{item.readback}</span></div></div>}
				  {item.error && <div className="change-error-note"><AlertTriangle size={17} /><div><strong>{item.status === 'stale' ? '为避免错误调价，本次没有写入' : '执行没有完成'}</strong><span>{item.error}</span></div></div>}
				  {item.status === 'pending' && <div className="change-actions"><Button variant="ghost" disabled={workingId === item.id} onClick={() => void runAction(item, 'reject')}><X size={16} />拒绝</Button><Button disabled={workingId === item.id} onClick={() => setConfirming(item)}><Check size={16} />确认应用</Button></div>}
				  {item.status === 'applied' && <div className="change-actions"><Button variant="secondary" disabled={workingId === item.id} onClick={() => void runAction(item, 'rollback')}><RefreshCw size={16} />安全撤回</Button><small>仅当目标当前值仍等于本次写入值时执行，避免覆盖人工后续修改。</small></div>}
                </div>}
              </article>
            );
          })}
        </div>
      )}
      <Modal open={!!confirming} onClose={() => workingId === null && setConfirming(null)} title="确认应用到生产配置" description="系统将在写入前重新采集并校验；证据变化时会停止写入并生成最新建议。" footer={<><Button variant="ghost" disabled={workingId !== null} onClick={() => setConfirming(null)}>取消</Button><Button disabled={workingId !== null} onClick={() => { if (confirming) void runAction(confirming, 'apply'); }}>{workingId !== null ? <LoaderCircle size={16} className="spin" /> : <ShieldCheck size={16} />}{workingId !== null ? '校验、写入并读回中' : '确认并应用'}</Button></>}>
		{confirming && <div className="confirmation"><div className="confirmation-target"><span>目标</span><strong>{confirming.target}</strong></div><div className="diff compact"><div><span>当前</span><code>{confirming.before}</code></div><ArrowRight size={18} /><div><span>写入</span><code>{confirming.after}</code></div></div>{confirming.pricingMode === 'group_model_contract' && <div className="confirmation-contract"><strong>{confirming.changedModels ?? confirming.modelChanges?.length ?? 0} 个模型将按各自真实计费单位原子更新</strong><span>最低毛利 {listingPercent(confirming.minimumMargin)}；写入前再次校验当前 revision，任何并发修改都会停止本次执行。</span></div>}<label className="check-row"><input type="checkbox" defaultChecked />若读回结果不一致，自动执行条件回滚</label></div>}
      </Modal>
		<Section title="自动执行日志" description="按时间和类型查看采集、镜像、调价、上架与授权结果；删除只影响日志记录" action={<Button variant="secondary" onClick={() => setActivityRefresh((value) => value + 1)} disabled={activityLoading}><RefreshCw size={15} className={activityLoading ? 'spin' : ''} />刷新</Button>}>
			<div className="activity-log-filters">
				<label><span>开始时间</span><input type="datetime-local" value={activityFrom} onChange={(event) => { setActivityFrom(event.target.value); resetActivityPaging(); }} /></label>
				<label><span>结束时间</span><input type="datetime-local" value={activityTo} onChange={(event) => { setActivityTo(event.target.value); resetActivityPaging(); }} /></label>
				<label><span>日志分类</span><select value={activityCategory} onChange={(event) => { const value = event.target.value as '' | ActivityRecord['kind']; setActivityCategory(value); if (value && value === activityExcludedCategory) setActivityExcludedCategory(''); resetActivityPaging(); }}><option value="">全部分类</option>{activityCategoryOptions.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>
				<label><span>执行状态</span><select value={activityStatus} onChange={(event) => { setActivityStatus(event.target.value as ActivityStatusFilter); resetActivityPaging(); }}>{activityStatusOptions.map((option) => <option key={option.value || 'all'} value={option.value}>{option.label}</option>)}</select></label>
				<label><span>屏蔽分类</span><select value={activityExcludedCategory} onChange={(event) => { const value = event.target.value as '' | ActivityRecord['kind']; setActivityExcludedCategory(value); if (value && value === activityCategory) setActivityCategory(''); resetActivityPaging(); }}><option value="">不屏蔽</option>{activityCategoryOptions.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>
				<button type="button" className="text-button activity-reset-filter" onClick={resetActivityFilters} disabled={!hasActivityFilters}><Filter size={14} />重置筛选</button>
			</div>
			<div className="activity-log-toolbar">
				<label><input type="checkbox" checked={allCurrentActivitiesSelected} onChange={(event) => toggleCurrentActivities(event.target.checked)} disabled={activityItems.length === 0} /><span>{allCurrentActivitiesSelected ? '取消本页全选' : '全选本页'}</span></label>
				<span>共 {activityTotal.toLocaleString('zh-CN')} 条 · 已选 {selectedActivityIDs.size} 条</span>
				<div>
					<Button variant="secondary" disabled={selectedActivityIDs.size === 0 || activityDeleting} onClick={() => setActivityDeleteConfirmation({ mode: 'selected', count: selectedActivityIDs.size })}><Trash2 size={15} />删除选中</Button>
					<Button variant="danger" disabled={activityTotal === 0 || activityDeleting} onClick={() => setActivityDeleteConfirmation({ mode: 'filtered', count: Math.min(500, activityTotal) })}><Trash2 size={15} />清空当前筛选</Button>
				</div>
			</div>
			{activityNotice && <div className="activity-log-notice success"><Check size={16} /><span>{activityNotice}</span></div>}
			{activityError && <div className="activity-log-notice error"><AlertTriangle size={16} /><span>{activityError}</span></div>}
			{activityLoading && activityItems.length === 0 ? <div className="activity-log-loading"><LoaderCircle size={20} className="spin" />正在读取日志…</div> : activityItems.length === 0 ? <EmptyState icon={<Activity size={22} />} title="当前条件下没有日志" description="调整时间、分类或状态筛选后再试。" /> : <div className={`timeline activity-timeline ${activityLoading ? 'is-refreshing' : ''}`}>{activityItems.map((activity) => <div className="timeline-item" key={activity.id}><label className="activity-row-select" aria-label={`选择日志 ${activity.title}`}><input type="checkbox" checked={selectedActivityIDs.has(activity.id)} onChange={(event) => toggleActivity(activity.id, event.target.checked)} /></label><span className={`timeline-dot ${activity.status}`} /><div><strong>{activity.title}</strong><p>{activity.detail}</p><ActivityDiagnostic activity={activity} /></div><time dateTime={activity.at}>{formatDateTime(activity.at)}</time></div>)}</div>}
			<div className="activity-pagination">
				<label><span>每页</span><select value={activityLimit} onChange={(event) => { setActivityLimit(Number(event.target.value)); resetActivityPaging(); }}><option value={20}>20 条</option><option value={30}>30 条</option><option value={50}>50 条</option><option value={100}>100 条</option></select></label>
				<span>第 {Math.min(activityPage, activityPageCount)} / {activityPageCount} 页</span>
				<div><Button variant="secondary" disabled={activityPage <= 1 || activityLoading} onClick={() => setActivityPage((value) => Math.max(1, value - 1))}><ChevronLeft size={15} />上一页</Button><Button variant="secondary" disabled={activityPage >= activityPageCount || activityLoading} onClick={() => setActivityPage((value) => Math.min(activityPageCount, value + 1))}>下一页<ChevronRight size={15} /></Button></div>
			</div>
		</Section>
		<Modal open={!!activityDeleteConfirmation} onClose={() => { if (!activityDeleting) setActivityDeleteConfirmation(null); }} title={activityDeleteConfirmation?.mode === 'selected' ? '删除选中的日志' : '清空当前筛选结果'} description="这只会删除 RelayOps 的活动日志，不会删除规则、变更记录、账本或远端配置。" footer={<><Button variant="ghost" disabled={activityDeleting} onClick={() => setActivityDeleteConfirmation(null)}>取消</Button><Button variant="danger" disabled={activityDeleting} onClick={() => void deleteActivities()}>{activityDeleting ? <LoaderCircle size={16} className="spin" /> : <Trash2 size={16} />}{activityDeleting ? '正在删除' : '确认删除'}</Button></>}>
			<div className="activity-delete-confirmation"><strong>本次最多删除 {activityDeleteConfirmation?.count ?? 0} 条</strong><span>{activityDeleteConfirmation?.mode === 'selected' ? '仅删除你在当前页面勾选的记录。' : `按当前${hasActivityFilters ? '筛选条件' : '全部日志'}从最新记录开始删除，单次上限 500 条。`}</span></div>
		</Modal>
    </>
  );
}
