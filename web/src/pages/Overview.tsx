import { useEffect, useMemo, useRef, useState } from 'react';
import {
  Activity,
  ArrowDownRight,
  ArrowRight,
  ArrowUpRight,
	CheckSquare2,
  CircleDollarSign,
  Clock3,
	EyeOff,
	Landmark,
	Maximize2,
	Minimize2,
	MonitorUp,
  Network,
	RotateCcw,
  ShieldCheck,
	SlidersHorizontal,
  Sparkles,
  TriangleAlert,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { Activity as ActivityRecord, BootstrapData, OverviewIntelligenceSnapshot, UnifiedMoney, UnifiedRatio } from '../api/types';
import { Button, formatDateTime, formatMoney, formatRunway, IconButton, Modal, Section, StatusBadge } from '../components/ui';
import type { PageId } from '../components/Shell';
import {
	OVERVIEW_METRIC_IDS,
	defaultOverviewPreferences,
	loadOverviewPreferences,
	saveOverviewPreferences,
	type OverviewMetricId,
	type OverviewViewMode,
} from './overviewPreferences';

const operatingMetricDefinitions: ReadonlyArray<{ id: OverviewMetricId; label: string; description: string }> = [
	{ id: 'user_balance', label: '本站用户总余额', description: '本站 NewAPI 用户剩余额度折算后的现金口径。' },
	{ id: 'today_user_spend', label: '今日用户总消耗', description: '本站今日用户扣费；可排除本站管理员、测试和内部调用。' },
	{ id: 'upstream_balance', label: '上游总余额', description: '全部启用供应商账户余额按充值系数折算后的合计。' },
	{ id: 'upstream_daily_spend', label: '上游近 24h 消耗', description: '供应商账户近 24 小时总消耗，无法按本站用户身份拆分外站使用。' },
	{ id: 'today_gross_profit', label: '今日现金毛利', description: '成本完整时显示严格结果；否则按同窗已匹配请求的加权成本率显示估算。' },
	{ id: 'today_gross_margin', label: '今日加权平均利润率', description: '按已匹配请求收入加权的严格利润率，不把缺失成本当作零。' },
	{ id: 'operating_gap_estimate', label: '运营消耗差额估算', description: '今日用户消耗与供应商近 24 小时消耗之间的运营估算。' },
	{ id: 'operating_gap_margin', label: '运营差额率估算', description: '基于消耗差额的估算比例，不等同于严格毛利率。' },
];

type OperatingMetricTone = '' | 'positive' | 'warning' | 'negative' | 'unknown';
type OperatingMetricCard = {
	id: OverviewMetricId;
	label: string;
	description: string;
	value: string;
	note: string;
	compactNote: string;
	tone: OperatingMetricTone;
	hasValue: boolean;
};

type OverviewActivityGroup = {
	key: string;
	primary: ActivityRecord;
	items: ActivityRecord[];
	repeatCount: number;
};

function activityRepeatCount(activity: ActivityRecord) {
	const parsed = Number(activity.metadata?.repeat_count ?? '1');
	return Number.isFinite(parsed) && parsed > 0 ? Math.floor(parsed) : 1;
}

function groupOverviewActivities(activities: ActivityRecord[]) {
	const groups = new Map<string, OverviewActivityGroup>();
	for (const activity of activities) {
		const key = [activity.rawKind, activity.status, activity.title, activity.detail].join('\u0000');
		const existing = groups.get(key);
		if (existing) {
			existing.items.push(activity);
			existing.repeatCount += activityRepeatCount(activity);
			continue;
		}
		groups.set(key, { key, primary: activity, items: [activity], repeatCount: activityRepeatCount(activity) });
	}
	return [...groups.values()];
}

function hasMoneyDisplayValue(metric?: UnifiedMoney | null) {
	if (!metric || metric.amount === undefined || metric.amount.trim() === '') return false;
	return Number.isFinite(Number(metric.amount));
}

function hasRatioDisplayValue(metric?: UnifiedRatio | null) {
	return metric?.value !== undefined && metric.value.trim() !== '' && Number.isFinite(Number(metric.value));
}

function operatingMoney(metric?: UnifiedMoney | null) {
	if (!metric || (!metric.amount && metric.known_items === 0)) return '未知';
	const value = Number(metric.amount);
	if (!Number.isFinite(value)) return '未知';
	const symbol = metric.currency === 'CNY' ? '¥/$' : metric.currency === 'USD' ? '$' : metric.currency;
	const formatted = `${symbol} ${value.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
	return metric.available ? formatted : `已知 ${formatted}`;
}

function operatingRatio(metric?: UnifiedRatio | null) {
	if (!metric?.available || metric.value === undefined) return '未知';
	const value = Number(metric.value);
	return Number.isFinite(value) ? `${(value * 100).toFixed(2)}%` : '未知';
}

function operatingEstimatedRatio(metric?: UnifiedRatio | null) {
	if (!metric || metric.value === undefined) return '未知';
	const value = Number(metric.value);
	if (!Number.isFinite(value)) return '未知';
	const output = `${(value * 100).toFixed(2)}%`;
	return metric.available ? output : `已知估算 ${output}`;
}

function operatingMarginEvidence(metric?: UnifiedRatio | null) {
	if (!metric) return '暂无数据';
	if (!metric.available) return metric.note ?? '暂无可验证匹配样本';
	const coverage = metric.coverage ? ` · 匹配覆盖 ${(Number(metric.coverage) * 100).toFixed(1)}%` : '';
	return `按已匹配请求加权${coverage} · 管理员过滤与分母一致`;
}

export function OverviewPage({ data, onNavigate }: { data: BootstrapData; onNavigate: (page: PageId) => void }) {
  const { metrics } = data;
	const [intelligence, setIntelligence] = useState<OverviewIntelligenceSnapshot | null>(null);
	const [financialStatus, setFinancialStatus] = useState<'loading' | 'ready' | 'error'>('loading');
	const [financialError, setFinancialError] = useState('');
	const financialRefreshInFlight = useRef(false);
	const activityRefreshInFlight = useRef(false);
	const [activities, setActivities] = useState<ActivityRecord[]>(() => data.activities);
	const [overviewPreferences, setOverviewPreferences] = useState(loadOverviewPreferences);
	const [overviewSettingsOpen, setOverviewSettingsOpen] = useState(false);

	useEffect(() => {
		let active = true;
		const refreshFinancial = () => {
			if (!active || document.visibilityState !== 'visible' || financialRefreshInFlight.current) return;
			financialRefreshInFlight.current = true;
			void api.overviewIntelligence().then((result) => {
				if (!active) return;
				setIntelligence(result.data);
				setFinancialError('');
				setFinancialStatus('ready');
			}).catch((error) => {
				if (!active) return;
				setFinancialError(getErrorMessage(error));
				setFinancialStatus((current) => current === 'loading' ? 'error' : current);
			}).finally(() => {
				financialRefreshInFlight.current = false;
			});
		};
		const refreshActivities = () => {
			if (!active || document.visibilityState !== 'visible' || activityRefreshInFlight.current) return;
			activityRefreshInFlight.current = true;
			void api.listActivities({ page: 1, limit: 20, include_total: false }).then((result) => {
				if (active) setActivities(result.data.items);
			}).catch(() => undefined).finally(() => {
				activityRefreshInFlight.current = false;
			});
		};
		const refresh = () => {
			refreshFinancial();
			refreshActivities();
		};
		refresh();
		const interval = window.setInterval(refresh, 60_000);
		const onFocus = () => refresh();
		const onVisibility = () => { if (document.visibilityState === 'visible') refresh(); };
		window.addEventListener('focus', onFocus);
		document.addEventListener('visibilitychange', onVisibility);
		return () => {
			active = false;
			window.clearInterval(interval);
			window.removeEventListener('focus', onFocus);
			document.removeEventListener('visibilitychange', onVisibility);
		};
	}, []);
	useEffect(() => setActivities(data.activities), [data.activities]);
	useEffect(() => saveOverviewPreferences(overviewPreferences), [overviewPreferences]);

	const operations = intelligence?.operations ?? null;
	const hasLedger = (intelligence?.raw_event_count ?? 0) > 0;
	const financialMetric = operations ? operatingMoney(operations.today_user_spend) : financialStatus === 'error' ? '暂不可用' : '—';
	const marginValue = operations?.today_gross_margin.available ? Number(operations.today_gross_margin.value) * 100 : null;
  const marginTone = (marginValue ?? 0) >= 15 ? 'positive' : (marginValue ?? 0) >= 5 ? 'warning' : 'negative';
	const operatingMetrics = useMemo<OperatingMetricCard[]>(() => {
		if (!operations) return [];
		const values: Record<OverviewMetricId, Omit<OperatingMetricCard, 'id' | 'label' | 'description'>> = {
			user_balance: {
				value: operatingMoney(operations.user_balance),
				note: operations.user_balance.note ?? '本站用户剩余额度，按目标站 quota 现金因子换算。',
				compactNote: operations.user_balance.unknown_items > 0 ? `${operations.user_balance.unknown_items} 项待补` : '本站余额口径',
				tone: '',
				hasValue: hasMoneyDisplayValue(operations.user_balance),
			},
			today_user_spend: {
				value: operatingMoney(operations.today_user_spend),
				note: operations.today_user_spend.note ?? '今日用户实际扣费，与充值流水、用户余额变动分开统计。',
				compactNote: operations.admin_excluded ? '已排除本站管理员' : '包含本站管理员',
				tone: '',
				hasValue: hasMoneyDisplayValue(operations.today_user_spend),
			},
			upstream_balance: {
				value: operatingMoney(operations.upstream_balance),
				note: operations.upstream_balance.unknown_items > 0 ? `${operations.upstream_balance.unknown_items} 个上游金额待补` : '所有启用上游已按充值系数换算',
				compactNote: operations.upstream_balance.unknown_items > 0 ? `${operations.upstream_balance.unknown_items} 个上游待补` : '供应商账户口径',
				tone: '',
				hasValue: hasMoneyDisplayValue(operations.upstream_balance),
			},
			upstream_daily_spend: {
				value: operatingMoney(operations.upstream_daily_spend),
				note: operations.upstream_daily_spend.note ?? '所有启用上游的近 24 小时消耗；不能按本站用户身份拆分外站调用。',
				compactNote: operations.upstream_daily_spend.unknown_items > 0 ? `${operations.upstream_daily_spend.unknown_items} 个上游待补` : '供应商账户总消耗',
				tone: '',
				hasValue: hasMoneyDisplayValue(operations.upstream_daily_spend),
			},
			today_gross_profit: {
				value: operatingMoney(operations.today_gross_profit),
				note: operations.today_gross_profit.note ?? '采购成本证据完整后才提供严格现金毛利。',
				compactNote: operations.today_gross_profit.evidence === 'estimated' ? '加权成本率估算' : operations.today_gross_profit.available ? '严格口径' : '等待采购成本证据',
				tone: operations.today_gross_profit.evidence === 'estimated' ? 'warning' : operations.today_gross_profit.available ? 'positive' : 'unknown',
				hasValue: hasMoneyDisplayValue(operations.today_gross_profit),
			},
			today_gross_margin: {
				value: operatingRatio(operations.today_gross_margin),
				note: operatingMarginEvidence(operations.today_gross_margin),
				compactNote: operations.today_gross_margin.available ? `匹配覆盖 ${(Number(operations.today_gross_margin.coverage ?? 0) * 100).toFixed(1)}%` : '等待采购成本证据',
				tone: operations.today_gross_margin.available ? marginTone : 'unknown',
				hasValue: hasRatioDisplayValue(operations.today_gross_margin),
			},
			operating_gap_estimate: {
				value: operatingMoney(operations.operating_gap_estimate),
				note: operations.operating_gap_estimate.note ?? '只比较消耗口径，不把充值现金流混入。',
				compactNote: '消耗差额估算',
				tone: operations.operating_gap_estimate.available ? 'positive' : operations.operating_gap_estimate.amount ? 'warning' : 'unknown',
				hasValue: hasMoneyDisplayValue(operations.operating_gap_estimate),
			},
			operating_gap_margin: {
				value: operatingEstimatedRatio(operations.operating_gap_margin),
				note: operations.operating_gap_margin.note ?? '运营估算，不替代严格的请求级毛利率。',
				compactNote: '消耗差额率估算',
				tone: operations.operating_gap_margin.available ? 'positive' : operations.operating_gap_margin.value ? 'warning' : 'unknown',
				hasValue: hasRatioDisplayValue(operations.operating_gap_margin),
			},
		};
		return operatingMetricDefinitions.map((definition) => ({ ...definition, ...values[definition.id] }));
	}, [marginTone, operations]);
	const selectedMetricIds = useMemo(() => new Set(overviewPreferences.selectedMetricIds), [overviewPreferences.selectedMetricIds]);
	const selectedOperatingMetrics = operatingMetrics.filter((metric) => selectedMetricIds.has(metric.id));
	const foldedOperatingMetrics = overviewPreferences.hideUnavailable ? selectedOperatingMetrics.filter((metric) => !metric.hasValue) : [];
	const visibleOperatingMetrics = overviewPreferences.hideUnavailable ? selectedOperatingMetrics.filter((metric) => metric.hasValue) : selectedOperatingMetrics;
	const operatingGridCustomized = overviewPreferences.mode === 'compact' || visibleOperatingMetrics.length !== OVERVIEW_METRIC_IDS.length;
	const supplierAlerts = data.suppliers.filter((item) => item.status === 'warning' || item.status === 'error');
	const urgentBalances = data.balances.filter((item) => item.status === 'warning' || item.status === 'error');
	const supplierCredentialIds = new Set(data.suppliers.flatMap((supplier) => supplier.credentialId ? [supplier.credentialId] : []));
	const credentialAlerts = data.credentials.filter((item) => supplierCredentialIds.has(item.id) && item.status !== 'valid');
	const visibleActivities = overviewPreferences.activityFilter === 'attention'
		? activities.filter((activity) => activity.status === 'warning' || activity.status === 'error')
		: activities;
	const activityGroups = useMemo(() => groupOverviewActivities(visibleActivities).slice(0, 12), [visibleActivities]);
	const attentionCount = supplierAlerts.length + urgentBalances.length + credentialAlerts.length;
	const runtimeSummary = !data.backgroundWorkersEnabled
		? '后台采集调度当前未运行；请检查正式运行配置。'
		: data.automationEnabled
			? '采集、成本计算和无人值守写价都在运行。'
			: '采集与成本计算正在运行；无人值守写价当前关闭。';
	const setOverviewMode = (mode: OverviewViewMode) => setOverviewPreferences((current) => ({ ...current, mode }));
	const toggleOverviewMetric = (metricId: OverviewMetricId, checked: boolean) => {
		setOverviewPreferences((current) => {
			const next = new Set(current.selectedMetricIds);
			if (checked) next.add(metricId);
			else next.delete(metricId);
			return { ...current, selectedMetricIds: OVERVIEW_METRIC_IDS.filter((id) => next.has(id)) };
		});
	};
	const selectAllOverviewMetrics = () => setOverviewPreferences((current) => ({ ...current, selectedMetricIds: [...OVERVIEW_METRIC_IDS] }));
	const restoreOverviewDefaults = () => setOverviewPreferences(defaultOverviewPreferences());

  return (
    <>
      <header className="overview-heading">
        <div>
          <span className="eyebrow">实时经营视图</span>
          <h1>上午好，银子</h1>
		  <p>{runtimeSummary} 当前有 {attentionCount} 项需要留意。</p>
        </div>
		<div className="overview-heading-actions">
			<div className="updated-time"><Clock3 size={15} />经营数据更新于 {new Date(operations?.generated_at ?? data.generatedAt).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}</div>
			<Button variant="secondary" onClick={() => onNavigate('wallboard')}><MonitorUp size={16} />副屏模式</Button>
		</div>
      </header>

      <div className="metric-grid">
        <article className="metric-card accent-teal">
          <div className="metric-icon"><Network size={20} /></div>
          <div className="metric-label">上游健康</div>
          <div className="metric-value">{metrics.healthySuppliers}<small> / {metrics.supplierCount}</small></div>
          <div className="metric-foot positive"><ShieldCheck size={14} />{metrics.healthySuppliers} 个站点最近采集正常</div>
        </article>
        <article className="metric-card accent-blue">
          <div className="metric-icon"><CircleDollarSign size={20} /></div>
		  <div className="metric-label">今日用户消耗</div>
		  <div className="metric-value">{financialMetric}</div>
		  <div className={`metric-foot ${operations?.today_user_spend.available ? 'positive' : financialStatus === 'error' ? 'warning' : ''}`}>
			{operations ? <><ShieldCheck size={14} />{operations.admin_excluded ? '已排除管理员' : '已纳入管理员'} · {operations.today_user_spend.unknown_items > 0 ? `${operations.today_user_spend.unknown_items} 项待补` : '金额完整'}</>
				: financialStatus === 'loading' ? <><Activity size={14} />正在读取经营账本</>
					: financialStatus === 'error' ? <><TriangleAlert size={14} />经营账本状态读取失败</> : '尚无经营账本事件'}
		  </div>
        </article>
        <article className="metric-card accent-amber">
          <div className="metric-icon"><TriangleAlert size={20} /></div>
          <div className="metric-label">待处理事项</div>
		  <div className="metric-value">{attentionCount + metrics.pendingChanges}</div>
          <div className="metric-foot warning">{metrics.pendingChanges} 项变更等待确认</div>
        </article>
        <article className="metric-card accent-violet">
          <div className="metric-icon"><Sparkles size={20} /></div>
          <div className="metric-label">自动化规则</div>
          <div className="metric-value">{metrics.automatedPolicies}</div>
          <div className="metric-foot"><Activity size={14} />当前启用的自动执行规则</div>
        </article>
      </div>

		<Section
			title="核心经营数据"
			description={operations
				? `统一按 ${operations.display_currency} 展示，CNY/USD 当前按 1:1 合并；${operations.admin_excluded ? '已排除本站管理员、测试和内部调用' : '当前包含本站管理员调用'}；上游余额与消耗仍按供应商账户口径`
				: '金额与事件条数分开；未知值不会按 0 参与利润'}
			action={<div className="overview-kpi-toolbar">
				<div className="segmented overview-view-switch" role="tablist" aria-label="核心经营数据视图">
					<button type="button" role="tab" aria-selected={overviewPreferences.mode === 'compact'} className={overviewPreferences.mode === 'compact' ? 'active' : ''} onClick={() => setOverviewMode('compact')}><Minimize2 size={14} />紧凑</button>
					<button type="button" role="tab" aria-selected={overviewPreferences.mode === 'detailed'} className={overviewPreferences.mode === 'detailed' ? 'active' : ''} onClick={() => setOverviewMode('detailed')}><Maximize2 size={14} />详细</button>
				</div>
				<IconButton label="自定义核心经营指标" onClick={() => setOverviewSettingsOpen(true)}><SlidersHorizontal size={17} /></IconButton>
				<button type="button" className="text-button" onClick={() => onNavigate('intelligence')}>查看分组与上游明细 <ArrowRight size={15} /></button>
			</div>}
		>
			{operations ? <>
				{visibleOperatingMetrics.length > 0 ? <div className={`operating-kpi-grid ${overviewPreferences.mode === 'compact' ? 'is-compact' : ''} ${operatingGridCustomized ? 'is-customized' : ''}`}>
					{visibleOperatingMetrics.map((metric) => <article key={metric.id} className={metric.tone} title={overviewPreferences.mode === 'compact' ? metric.note : undefined}>
						<span>{metric.label}</span>
						<strong>{metric.value}</strong>
						<small>{overviewPreferences.mode === 'compact' ? metric.compactNote : metric.note}</small>
					</article>)}
				</div> : <div className="overview-kpi-empty"><EyeOff size={20} /><div><strong>当前没有选中核心指标</strong><span>打开设置即可恢复默认组合或重新选择需要在首页关注的数据。</span></div><Button variant="secondary" onClick={() => setOverviewSettingsOpen(true)}><SlidersHorizontal size={15} />选择指标</Button></div>}
				{foldedOperatingMetrics.length > 0 && <div className="overview-folded-note"><EyeOff size={14} /><span>已折叠 {foldedOperatingMetrics.length} 项暂无结果</span><button type="button" className="text-button" onClick={() => setOverviewPreferences((current) => ({ ...current, hideUnavailable: false }))}>显示这些指标</button></div>}
			</> : financialStatus === 'loading' ? <div className="evidence-empty"><Activity size={20} /><div><strong>正在生成统一现金快照</strong><span>余额、消耗、充值折算和管理员过滤会在同一快照内完成。</span></div></div>
				: <div className="evidence-empty" title={financialError}><TriangleAlert size={20} /><div><strong>经营金额暂不可用</strong><span>上游采集与自动跟价不受影响，可进入经营智能重试。</span></div></div>}
		</Section>

		<Modal
			open={overviewSettingsOpen}
			title="首页核心指标"
			description="选择首页保留的数据。设置仅保存在当前浏览器，不会修改经营账本或自动化规则。"
			onClose={() => setOverviewSettingsOpen(false)}
			footer={<div className="overview-preference-footer">
				<Button variant="ghost" onClick={restoreOverviewDefaults}><RotateCcw size={15} />恢复默认</Button>
				<Button variant="secondary" onClick={selectAllOverviewMetrics}><CheckSquare2 size={15} />全选指标</Button>
				<Button onClick={() => setOverviewSettingsOpen(false)}>完成</Button>
			</div>}
		>
			<div className="overview-preferences">
				<label className="overview-availability-choice">
					<input type="checkbox" checked={overviewPreferences.hideUnavailable} onChange={(event) => setOverviewPreferences((current) => ({ ...current, hideUnavailable: event.target.checked }))} />
					<span><strong>隐藏暂无结果的指标</strong><small>只折叠完全没有数值的项目；已有估算值仍会显示，数据恢复后会自动重新出现。</small></span>
				</label>
				<div className="overview-choice-heading"><span>已选 {overviewPreferences.selectedMetricIds.length} / {OVERVIEW_METRIC_IDS.length}</span><small>取消选择的指标即使以后有值也不会在首页出现。</small></div>
				<div className="overview-metric-choice-grid">
					{operatingMetricDefinitions.map((metric) => {
						const checked = selectedMetricIds.has(metric.id);
						return <label key={metric.id} className={`overview-metric-choice ${checked ? 'is-selected' : ''}`}>
							<input type="checkbox" checked={checked} onChange={(event) => toggleOverviewMetric(metric.id, event.target.checked)} />
							<span><strong>{metric.label}</strong><small>{metric.description}</small></span>
						</label>;
					})}
				</div>
			</div>
		</Modal>

		<Section title="上游运行概览" description={`${data.suppliers.length} 个已配置站点，采集异常与授权问题优先显示`} action={<button className="text-button" onClick={() => onNavigate('suppliers')}>管理上游 <ArrowRight size={15} /></button>}>
			<div className="supplier-overview-strip">
				{[...data.suppliers].sort((a, b) => ({ error: 0, warning: 1, unknown: 2, healthy: 3 }[a.status] - { error: 0, warning: 1, unknown: 2, healthy: 3 }[b.status])).map((supplier) => (
					<button key={supplier.id} onClick={() => onNavigate('suppliers')}>
						<span className={`supplier-dot ${supplier.status}`} />
						<strong>{supplier.name}</strong>
						<small>{supplier.groupCount} 分组 · {supplier.modelCount} 模型</small>
					</button>
				))}
			</div>
		</Section>

      <div className="overview-grid">
        <Section title="需要关注" description="只展示会影响成本或连续运行的事项" action={<button className="text-button" onClick={() => onNavigate('balances')}>查看全部 <ArrowRight size={15} /></button>}>
		  <div className="attention-list">
			{supplierAlerts.map((supplier) => (
			  <button className="attention-row" key={`supplier-${supplier.id}`} onClick={() => onNavigate('suppliers')}>
				<span className={`attention-icon ${supplier.status}`}><Network size={18} /></span>
				<span className="attention-content"><strong>{supplier.name} 最近采集异常</strong><small>{supplier.note || '打开上游详情查看失败分类和最近一次成功时间'}</small></span>
				<span className="attention-value">查看详情<ArrowRight size={15} /></span>
			  </button>
			))}
			{urgentBalances.map((balance) => (
              <button className="attention-row" key={balance.id} onClick={() => onNavigate('balances')}>
                <span className={`attention-icon ${balance.status}`}><Landmark size={18} /></span>
				<span className="attention-content"><strong>{balance.supplier} 余额续航偏低</strong><small>按近期消耗预计可用 {formatRunway(balance.runwayHours)}</small></span>
				<span className="attention-value">{formatMoney(balance.balance, balance.currency)}<ArrowRight size={15} /></span>
              </button>
            ))}
			{credentialAlerts.map((credential) => (
              <button className="attention-row" key={credential.id} onClick={() => onNavigate('connectors')}>
                <span className="attention-icon warning"><ShieldCheck size={18} /></span>
                <span className="attention-content"><strong>{credential.supplier} 登录态需要刷新</strong><small>{credential.status === 'expiring' ? `预计 ${formatDateTime(credential.expiresAt)} 到期` : '当前无法读取登录后数据'}</small></span>
                <span className="attention-value">去处理<ArrowRight size={15} /></span>
              </button>
			))}
			{attentionCount === 0 && <div className="evidence-empty"><ShieldCheck size={20} /><div><strong>当前没有需要人工处理的事项</strong><span>余额未知样本会留在余额续航页继续采集，不会冒充低余额告警。</span></div></div>}
		  </div>
        </Section>

		<Section title="今日收支证据" description={hasLedger && operations ? `${new Date(operations.day_started_at).toLocaleString('zh-CN')} 起；金额、覆盖率和缺失项分开显示` : '独立核对用户扣费、请求级采购成本与利润'} action={<button className="text-button" onClick={() => onNavigate('intelligence')}>查看经营智能 <ArrowRight size={15} /></button>}>
          {operations ? <div className="finance-summary">
				<div><span>用户扣费</span><strong>{operatingMoney(operations.today_revenue)}</strong><small>{operations.today_revenue.unknown_items > 0 ? `${operations.today_revenue.unknown_items} 项未知` : '今日金额已知'}</small></div>
				<div><span>请求级采购成本</span><strong>{operatingMoney(operations.today_cost)}</strong><small>{operations.today_cost.unknown_items > 0 ? `${operations.today_cost.unknown_items} 条尚未关联` : '今日金额已知'}</small></div>
				<div><span>{operations.today_gross_profit.evidence === 'estimated' ? '毛利估算' : '严格毛利'} / 加权利润率</span><strong>{operatingMoney(operations.today_gross_profit)}</strong><small>{operatingRatio(operations.today_gross_margin)} · 严格成本覆盖 {(Number(operations.profit_data_quality.cost_coverage ?? 0) * 100).toFixed(1)}%</small></div>
				<div><span>运营差额估算</span><strong>{operatingMoney(operations.operating_gap_estimate)}</strong><small>{operatingEstimatedRatio(operations.operating_gap_margin)} · 只算消耗，不算充值</small></div>
			</div> : financialStatus === 'loading' ? <div className="evidence-empty"><Activity size={20} /><div><strong>正在核对经营账本</strong><span>总览先显示采集状态，账本读取完成后再展示经营结论。</span></div></div>
				: financialStatus === 'error' ? <div className="evidence-empty" title={financialError}><TriangleAlert size={20} /><div><strong>经营账本暂不可用</strong><span>价格监控与余额采集不受影响；进入经营智能页面可重新读取。</span></div></div>
					: <div className="evidence-empty"><CircleDollarSign size={20} /><div><strong>尚无经营账本事件</strong><span>同步收入与采购成本来源后显示；缺失数据不会展示为 0 元。</span></div></div>}
        </Section>
      </div>

      <Section title="重点价格" description="按价格变化与利润风险排序" action={<button className="text-button" onClick={() => onNavigate('watchlist')}>管理关注项 <ArrowRight size={15} /></button>}>
        <div className="table-wrap">
          <table>
            <thead><tr><th>模型 / 上游</th><th>上游分组</th><th>有效分组倍率</th><th>本站倍率</th><th>规则价差</th><th>24h 变化</th><th>状态</th></tr></thead>
            <tbody>
              {data.watchItems.map((item) => (
                <tr key={item.id}>
                  <td><strong>{item.model}</strong><small>{item.supplier}</small></td>
                  <td>{item.group}</td>
                  <td className="mono">{item.upstreamRate ?? '待获取'}</td>
                  <td className="mono">{item.localRate ?? '—'}</td>
                  <td className={item.grossMargin === null ? '' : item.grossMargin < 0 ? 'negative' : item.grossMargin < 12 ? 'warning' : 'positive'}>{item.localRate === null ? '待映射' : item.grossMargin === null ? '待计算' : `${item.grossMargin.toFixed(1)}%`}</td>
				  <td className={item.delta24h === null ? '' : item.delta24h > 0 ? 'negative' : item.delta24h < 0 ? 'positive' : ''}>
					{item.delta24h === null ? '样本不足' : <>{item.delta24h > 0 ? <ArrowUpRight size={14} /> : item.delta24h < 0 ? <ArrowDownRight size={14} /> : null}{Math.abs(item.delta24h).toFixed(2)}%</>}
				  </td>
                  <td><StatusBadge status={item.status} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Section>

	  <Section title="最近活动" description="系统自动采集、计算与变更记录" action={<div className="segmented overview-activity-filter" aria-label="最近活动筛选">
		<button type="button" className={overviewPreferences.activityFilter === 'all' ? 'active' : ''} onClick={() => setOverviewPreferences((current) => ({ ...current, activityFilter: 'all' }))}>全部</button>
		<button type="button" className={overviewPreferences.activityFilter === 'attention' ? 'active' : ''} onClick={() => setOverviewPreferences((current) => ({ ...current, activityFilter: 'attention' }))}>只看需关注</button>
	  </div>}>
		<div className="timeline">
		  {activityGroups.length === 0 && <div className="timeline-empty">当前筛选下没有活动</div>}
		  {activityGroups.map((group) => group.repeatCount > 1 ? (
			<details className="timeline-item timeline-group" key={group.key}>
			  <summary>
				<span className={`timeline-dot ${group.primary.status}`} />
				<div><strong>{group.primary.title}<span className="timeline-repeat-count">合并 {group.repeatCount} 次</span></strong><p>{group.primary.detail}</p></div>
				<time dateTime={group.primary.at}>{formatDateTime(group.primary.at)}</time>
			  </summary>
			  <div className="timeline-group-details">
				{group.items.map((activity) => <span key={activity.id}>{formatDateTime(activity.at)}</span>)}
				{group.repeatCount > group.items.length && <span>另有 {group.repeatCount - group.items.length} 次已由服务端合并记录</span>}
			  </div>
			</details>
		  ) : (
			<div className="timeline-item" key={group.key}>
			  <span className={`timeline-dot ${group.primary.status}`} />
			  <div><strong>{group.primary.title}</strong><p>{group.primary.detail}</p></div>
			  <time dateTime={group.primary.at}>{formatDateTime(group.primary.at)}</time>
			</div>
		  ))}
		</div>
	  </Section>
    </>
  );
}
