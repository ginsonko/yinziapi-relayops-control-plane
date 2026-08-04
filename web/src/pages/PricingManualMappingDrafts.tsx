import { useMemo, useState } from 'react';
import {
	AlertTriangle,
	Check,
	CheckCircle2,
	ChevronDown,
	CircleHelp,
	Database,
	GitBranch,
	LoaderCircle,
	RefreshCw,
	Save,
	Search,
	ShieldCheck,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type {
	ManualMappingDraftPreview,
	ManualMappingRuleInput,
	ManualMappingSourceOption,
	ManualMappingTargetGroup,
	PolicyMode,
	RawPricingPolicy,
	TargetSite,
} from '../api/types';
import { Button, EmptyState, Section, Toggle, modeLabel } from '../components/ui';

type ManualFilter = 'all' | ManualMappingTargetGroup['status'];

interface ManualDraft {
	candidateId: string;
	rule: ManualMappingRuleInput;
}

interface PricingManualMappingDraftsProps {
	targets: TargetSite[];
	targetId: string;
	onTargetChange: (targetId: string) => void;
	onReload: () => Promise<unknown>;
	onNotify: (message: string, tone?: 'success' | 'info' | 'error') => void;
}

const manualStatusCopy: Record<ManualMappingTargetGroup['status'], { label: string; detail: string }> = {
	unmapped: { label: '待配置', detail: '该本站分组尚未绑定上游跟价规则' },
	mapped: { label: '已有规则', detail: '已载入当前规则，可以直接修改并保存' },
	conflict: { label: '规则冲突', detail: '同一本站分组存在多个规则所有者，需先在价格策略中处理' },
};

const ruleTypes: PolicyMode[] = ['add', 'multiply', 'fixed', 'target_margin', 'markup', 'subsidy_rate'];

function formatManualTime(value?: string) {
	if (!value) return '未记录';
	const parsed = new Date(value);
	return Number.isNaN(parsed.getTime()) ? '未记录' : parsed.toLocaleString('zh-CN', { hour12: false });
}

function optionForDraft(group: ManualMappingTargetGroup, draft?: ManualDraft) {
	return group.options.find((option) => option.candidate_id === draft?.candidateId);
}

function defaultRule(option?: ManualMappingSourceOption): ManualMappingRuleInput {
	return {
		source_basis: 'group_ratio',
		rule_type: option?.recommended_rule_type ?? 'add',
		value: option?.recommended_value ?? '',
		profit_basis: 'contribution',
		revenue_cash_factor: '1',
		cost_cash_factor: '1',
		risk_buffer_rate: '0',
		allow_negative_profit: false,
		subsidy_budget_daily: '0',
		minimum_ratio: '0',
		maximum_ratio: '0',
		rounding_step: '0.001',
		only_follow_increase: false,
		only_follow_decrease: false,
		enabled: false,
		execution_mode: 'approval',
		coverage_mode: 'threshold',
		minimum_coverage_ratio: '0.8',
	};
}

function existingRule(policy: RawPricingPolicy): ManualMappingRuleInput {
	return {
		name: policy.name,
		source_basis: 'group_ratio',
		rule_type: policy.rule_type,
		value: policy.value,
		profit_basis: policy.profit_basis,
		revenue_cash_factor: policy.revenue_cash_factor,
		cost_cash_factor: policy.cost_cash_factor,
		risk_buffer_rate: policy.risk_buffer_rate,
		allow_negative_profit: policy.allow_negative_profit,
		subsidy_budget_daily: policy.subsidy_budget_daily,
		minimum_ratio: policy.minimum_ratio,
		maximum_ratio: policy.maximum_ratio,
		rounding_step: policy.rounding_step,
		only_follow_increase: policy.only_follow_increase,
		only_follow_decrease: policy.only_follow_decrease,
		enabled: policy.enabled,
		execution_mode: policy.execution_mode,
		coverage_mode: policy.coverage_mode,
		minimum_coverage_ratio: policy.minimum_coverage_ratio,
	};
}

function initialDrafts(preview: ManualMappingDraftPreview) {
	const drafts: Record<string, ManualDraft> = {};
	for (const group of preview.groups) {
		let option = group.options.find((candidate) => candidate.candidate_id === group.recommended_option_id);
		if (group.existing_policy) {
			const existing = group.existing_policy;
			option = group.options.find((candidate) => candidate.supplier_id === existing.supplier_id &&
				candidate.source_group.toLocaleLowerCase() === existing.source_group.toLocaleLowerCase()) ?? option;
		}
		option ??= group.options[0];
		drafts[group.target_group] = {
			candidateId: option?.candidate_id ?? '',
			rule: group.existing_policy ? existingRule(group.existing_policy) : defaultRule(option),
		};
	}
	return drafts;
}

function sourceSites(options: ManualMappingSourceOption[]) {
	const sites = new Map<number, string>();
	for (const option of options) sites.set(option.supplier_id, option.supplier_name);
	return Array.from(sites, ([id, name]) => ({ id, name })).sort((left, right) => left.name.localeCompare(right.name, 'zh-CN'));
}

function groupSearchText(group: ManualMappingTargetGroup) {
	return [
		group.target_group,
		...group.channels.flatMap((channel) => [channel.name, channel.base_origin ?? '', channel.tag ?? '', channel.remark ?? '']),
		...group.options.flatMap((option) => [option.supplier_name, option.source_group_name, option.source_group]),
	].join('\n').toLocaleLowerCase();
}

function sourceRatioEvidence(source: ManualMappingSourceOption) {
	const effective = source.source_ratio || '未知';
	const raw = source.raw_source_ratio || effective;
	const factor = source.recharge_value_factor || '1';
	return raw !== effective || factor !== '1'
		? `有效倍率 ${effective} · 原始 ${raw} × 现金折算 ${factor}`
		: `真实分组倍率 ${effective}`;
}

export function PricingManualMappingDrafts(props: PricingManualMappingDraftsProps) {
	const [preview, setPreview] = useState<ManualMappingDraftPreview | null>(null);
	const [drafts, setDrafts] = useState<Record<string, ManualDraft>>({});
	const [selected, setSelected] = useState<Set<string>>(new Set());
	const [filter, setFilter] = useState<ManualFilter>('all');
	const [query, setQuery] = useState('');
	const [loading, setLoading] = useState(false);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState('');
	const target = props.targets.find((item) => String(item.id) === props.targetId);

	const runPreview = async () => {
		if (!props.targetId) return;
		setLoading(true);
		setError('');
		try {
			const response = await api.previewManualMappingDrafts({ target_site_id: Number(props.targetId) });
			setPreview(response.data);
			setDrafts(initialDrafts(response.data));
			setSelected(new Set());
			setFilter('all');
		} catch (cause) {
			setError(getErrorMessage(cause));
		} finally {
			setLoading(false);
		}
	};

	const changeTarget = (value: string) => {
		props.onTargetChange(value);
		setPreview(null);
		setDrafts({});
		setSelected(new Set());
		setError('');
	};

	const visibleGroups = useMemo(() => {
		const normalized = query.trim().toLocaleLowerCase();
		return preview?.groups.filter((group) => {
			if (filter !== 'all' && group.status !== filter) return false;
			return normalized === '' || groupSearchText(group).includes(normalized);
		}) ?? [];
	}, [filter, preview, query]);

	const updateRule = (targetGroup: string, patch: Partial<ManualMappingRuleInput>) => {
		setDrafts((current) => ({
			...current,
			[targetGroup]: { ...current[targetGroup], rule: { ...current[targetGroup].rule, ...patch } },
		}));
	};

	const chooseOption = (group: ManualMappingTargetGroup, option: ManualMappingSourceOption, resetRule: boolean) => {
		setDrafts((current) => ({
			...current,
			[group.target_group]: {
				candidateId: option.candidate_id,
				rule: resetRule ? { ...current[group.target_group].rule, rule_type: option.recommended_rule_type ?? 'add', value: option.recommended_value ?? '' } : current[group.target_group].rule,
			},
		}));
	};

	const toggleGroup = (group: ManualMappingTargetGroup) => {
		if (group.status === 'conflict' || !optionForDraft(group, drafts[group.target_group])) return;
		setSelected((current) => {
			const next = new Set(current);
			if (next.has(group.target_group)) next.delete(group.target_group);
			else next.add(group.target_group);
			return next;
		});
	};

	const saveSelected = async () => {
		if (!preview || selected.size === 0) return;
		const groups = preview.groups.filter((group) => selected.has(group.target_group));
		for (const group of groups) {
			const draft = drafts[group.target_group];
			const option = optionForDraft(group, draft);
			if (!option || !draft.rule.value.trim()) {
				setError(`${group.target_group} 缺少上游分组或规则参数，无法保存。`);
				return;
			}
			if (draft.rule.enabled && draft.rule.execution_mode === 'auto' && target?.write_mode !== 'auto') {
				setError(`${group.target_group} 选择了自动执行，但本站目标尚未允许自动写入。`);
				return;
			}
		}
		setSaving(true);
		setError('');
		try {
			const response = await api.materializeManualMappingDrafts({
				target_site_id: preview.target_site_id,
				target_evidence_hash: preview.target_evidence_hash,
				selections: groups.map((group) => {
					const draft = drafts[group.target_group];
					const option = optionForDraft(group, draft)!;
					return {
						candidate_id: option.candidate_id,
						supplier_id: option.supplier_id,
						source_group: option.source_group,
						source_snapshot_hash: option.source_snapshot_hash,
						target_group: group.target_group,
						expected_policy_id: group.existing_policy?.id,
						expected_policy_updated_at: group.existing_policy?.updated_at,
						confirm_manual: true,
						rule: draft.rule,
					};
				}),
			});
			const created = response.data.policies.filter((item) => item.created).length;
			const updated = response.data.policies.filter((item) => item.updated).length;
			const unchanged = response.data.policies.filter((item) => item.idempotent).length;
			props.onNotify(`已原子保存 ${response.data.policies.length} 条规则：新建 ${created}、更新 ${updated}${unchanged ? `、无需变更 ${unchanged}` : ''}`, 'success');
			await props.onReload();
			await runPreview();
		} catch (cause) {
			const message = getErrorMessage(cause);
			setError(message);
			props.onNotify(message, 'error');
		} finally {
			setSaving(false);
		}
	};

	return <>
		<div className="mapping-flow manual-mapping-flow" aria-label="按本站渠道配置工作流">
			<div><Database size={18} /><span>读取本站渠道</span><small>按真实分组聚合渠道线索</small></div>
			<i />
			<div><GitBranch size={18} /><span>人工选择上游</span><small>候选排序只提供参考</small></div>
			<i />
			<div><ShieldCheck size={18} /><span>原子保存规则</span><small>失败整批回滚，不直接改价</small></div>
		</div>

		<Section title="本站渠道范围" description="默认选择已连接的本站 NewAPI；每个本站分组只会形成一条价格规则，渠道名称用于帮助识别真实上游。" action={<Button onClick={() => void runPreview()} disabled={!props.targetId || loading}>{loading ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}{loading ? '正在读取渠道' : preview ? '刷新渠道与上游' : '读取本站渠道'}</Button>}>
			<div className="manual-mapping-controls">
				<label><span>本站目标</span><select value={props.targetId} onChange={(event) => changeTarget(event.target.value)}><option value="">请选择</option>{props.targets.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></label>
				<div className={`manual-write-gate ${target?.write_mode === 'auto' ? 'ready' : ''}`}><ShieldCheck size={17} /><span><strong>{target?.write_mode === 'auto' ? '目标允许自动写入' : '目标保持审批写入'}</strong><small>新规则默认禁用且需要审批；只有你显式选择自动并启用时才受目标写入门禁约束。</small></span></div>
			</div>
			{props.targets.length === 0 && <div className="mapping-inline-warning"><AlertTriangle size={17} /><span>尚未连接可用的本站 NewAPI 目标，请先在“站点接入”完成连接。</span></div>}
			{error && <div className="mapping-inline-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
		</Section>

		{!preview && !loading && props.targets.length > 0 && <EmptyState icon={<GitBranch size={25} />} title="尚未读取本站渠道" description="点击“读取本站渠道”后，系统会用真实渠道名称、分组、上游地址和模型帮助你逐项绑定，不会向本站发送写请求。" />}

		{preview && <>
			<div className="mapping-summary manual-mapping-summary">
				<div><strong>{preview.summary.target_groups}</strong><span>本站分组</span></div>
				<div><strong>{preview.summary.target_channels}</strong><span>本站渠道</span></div>
				<div><strong>{preview.summary.unmapped}</strong><span>待配置</span></div>
				<div><strong>{preview.summary.mapped}</strong><span>已有规则</span></div>
				<div><strong>{preview.summary.suppliers_usable}/{preview.summary.suppliers_requested}</strong><span>上游目录可用</span></div>
			</div>

			{preview.supplier_issues.length > 0 && <div className="mapping-issues"><AlertTriangle size={17} /><div><strong>{preview.supplier_issues.length} 个上游缺少可信目录</strong>{preview.supplier_issues.map((issue) => <span key={issue.supplier_id}>{issue.supplier_name || `#${issue.supplier_id}`}：{issue.message}</span>)}</div></div>}

			<Section title="已有渠道映射" description={`目标证据 ${preview.target_evidence_hash.slice(0, 10)} · 实时读取于 ${formatManualTime(preview.generated_at)}`} action={<div className="manual-mapping-toolbar"><label><Search size={14} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索分组、渠道或上游" aria-label="搜索本站渠道映射" /></label><div className="mapping-filters">{(['all', 'unmapped', 'mapped', 'conflict'] as ManualFilter[]).map((value) => <button type="button" key={value} className={filter === value ? 'active' : ''} onClick={() => setFilter(value)}>{value === 'all' ? '全部' : manualStatusCopy[value].label}</button>)}</div></div>}>
				<div className="manual-mapping-list">
					{visibleGroups.map((group) => {
						const draft = drafts[group.target_group];
						const chosen = optionForDraft(group, draft);
						const sites = sourceSites(group.options);
						const siteOptions = group.options.filter((option) => option.supplier_id === chosen?.supplier_id);
						const isSelected = selected.has(group.target_group);
						const selectable = group.status !== 'conflict' && Boolean(chosen);
						const automaticBlocked = draft?.rule.enabled && draft.rule.execution_mode === 'auto' && target?.write_mode !== 'auto';
						return <article className={`manual-mapping-row status-${group.status} ${isSelected ? 'is-selected' : ''}`} key={group.target_group}>
							<button type="button" className="mapping-check" aria-label={isSelected ? `取消选择 ${group.target_group}` : `选择 ${group.target_group}`} title={selectable ? (isSelected ? '取消选择' : '加入本次原子保存') : manualStatusCopy[group.status].detail} disabled={!selectable} onClick={() => toggleGroup(group)}>{isSelected && <Check size={16} />}</button>
							<div className="manual-target-group"><span className={`mapping-status status-${group.status}`}>{group.status === 'mapped' ? <CheckCircle2 size={13} /> : group.status === 'conflict' ? <AlertTriangle size={13} /> : <CircleHelp size={13} />}{manualStatusCopy[group.status].label}</span><strong>{group.target_group}</strong><small>本站倍率 {group.target_ratio || '待读取'} · {group.models.length} 个模型 · {group.channels.length} 条渠道</small></div>
							<div className="manual-channel-clues">
								{group.channels.slice(0, 3).map((channel) => <div key={channel.id}><i className={channel.status === 1 ? 'healthy' : 'disabled'} /><span><strong>{channel.name || `渠道 #${channel.id}`}</strong><small>#{channel.id} · {channel.base_origin || '未配置上游地址'}{channel.status === 1 ? '' : ' · 已停用'}</small></span></div>)}
								{group.channels.length > 3 && <small>另有 {group.channels.length - 3} 条同分组渠道</small>}
							</div>
							<div className="manual-source-pickers">
								<label><span>上游站点</span><select value={chosen?.supplier_id ?? ''} disabled={group.status === 'conflict' || sites.length === 0} onChange={(event) => { const option = group.options.find((item) => item.supplier_id === Number(event.target.value)); if (option) chooseOption(group, option, true); }}><option value="">请选择</option>{sites.map((site) => <option value={site.id} key={site.id}>{site.name}</option>)}</select></label>
								<label><span>上游分组</span><select value={chosen?.candidate_id ?? ''} disabled={group.status === 'conflict' || siteOptions.length === 0} onChange={(event) => { const option = group.options.find((item) => item.candidate_id === event.target.value); if (option) chooseOption(group, option, true); }}><option value="">请选择</option>{siteOptions.map((option) => <option value={option.candidate_id} key={`${option.candidate_id}-${option.source_group}`}>{option.source_group_name || option.source_group} · {sourceRatioEvidence(option)}</option>)}</select></label>
							</div>
							<div className="manual-rule-main">
								<label><span>跟价规则</span><select value={draft?.rule.rule_type ?? 'add'} disabled={group.status === 'conflict'} onChange={(event) => updateRule(group.target_group, { rule_type: event.target.value as PolicyMode })}>{ruleTypes.map((ruleType) => <option value={ruleType} key={ruleType}>{modeLabel(ruleType)}</option>)}</select></label>
								<label><span>规则参数</span><input value={draft?.rule.value ?? ''} disabled={group.status === 'conflict'} placeholder={chosen?.recommended_value || '请输入'} onChange={(event) => updateRule(group.target_group, { value: event.target.value })} /></label>
								<label><span>执行方式</span><select value={draft?.rule.execution_mode ?? 'approval'} disabled={group.status === 'conflict'} onChange={(event) => updateRule(group.target_group, { execution_mode: event.target.value as 'approval' | 'auto' })}><option value="approval">生成建议后确认</option><option value="auto" disabled={target?.write_mode !== 'auto'}>符合门禁时自动执行</option></select></label>
								<label className="manual-rule-toggle"><span>启用规则</span><span><Toggle checked={draft?.rule.enabled ?? false} disabled={group.status === 'conflict'} label={`${draft?.rule.enabled ? '停用' : '启用'} ${group.target_group}`} onChange={(enabled) => updateRule(group.target_group, { enabled })} />{draft?.rule.enabled ? '已启用' : '保存为停用'}</span></label>
							</div>
							{chosen && <div className="manual-option-evidence"><span>{chosen.candidate_id === group.recommended_option_id ? '系统首选候选' : '管理员手动选择'}</span><small>{sourceRatioEvidence(chosen)} · 等价成本 {chosen.equivalent_cost_ratio || '待核算'} · 模型重叠 {Math.round(Number(chosen.model_jaccard || 0) * 100)}% · 综合证据 {Math.round(Number(chosen.score || 0) * 100)} / 100 · 快照 {chosen.source_fresh ? '新鲜' : '已过期'} · {chosen.origin_match ? '与本站渠道同源' : '非同源候选'}</small></div>}
							{automaticBlocked && <div className="manual-row-warning"><AlertTriangle size={14} />本站目标未授权自动写入，请改为审批执行或先在“站点接入”开启写入门禁。</div>}
							{group.status === 'conflict' && <div className="manual-row-warning"><AlertTriangle size={14} />规则 {group.conflict_policy_ids?.join('、')} 同时拥有此分组，人工映射不会猜测覆盖对象。</div>}
							<details className="manual-mapping-details">
								<summary>高级规则与渠道证据 <ChevronDown size={14} /></summary>
								<div>
									<div className="manual-advanced-grid">
										<label><span>规则名称</span><input value={draft?.rule.name ?? ''} placeholder="留空自动生成" onChange={(event) => updateRule(group.target_group, { name: event.target.value })} /></label>
										<label><span>跟价来源口径</span><strong>上游真实分组倍率</strong><small>逐模型价格由分组镜像同步。</small></label>
										<label><span>利润口径</span><select value={draft?.rule.profit_basis ?? 'contribution'} onChange={(event) => updateRule(group.target_group, { profit_basis: event.target.value as ManualMappingRuleInput['profit_basis'] })}><option value="contribution">贡献利润</option><option value="cash">现金利润</option><option value="nominal">名义利润</option></select></label>
										<label><span>最低 / 最高倍率</span><span className="manual-paired-input"><input value={draft?.rule.minimum_ratio ?? '0'} onChange={(event) => updateRule(group.target_group, { minimum_ratio: event.target.value })} /><input value={draft?.rule.maximum_ratio ?? '0'} onChange={(event) => updateRule(group.target_group, { maximum_ratio: event.target.value })} /></span></label>
										<label><span>取整步长</span><input value={draft?.rule.rounding_step ?? '0.001'} onChange={(event) => updateRule(group.target_group, { rounding_step: event.target.value })} /></label>
										<label><span>最低价格覆盖率</span><input value={draft?.rule.minimum_coverage_ratio ?? '0.8'} onChange={(event) => updateRule(group.target_group, { minimum_coverage_ratio: event.target.value })} /></label>
										<label><span>风险缓冲率</span><input value={draft?.rule.risk_buffer_rate ?? '0'} onChange={(event) => updateRule(group.target_group, { risk_buffer_rate: event.target.value })} /></label>
									</div>
									<div className="manual-rule-flags"><label><input type="checkbox" checked={draft?.rule.only_follow_increase ?? false} onChange={(event) => updateRule(group.target_group, { only_follow_increase: event.target.checked })} />只跟随涨价</label><label><input type="checkbox" checked={draft?.rule.only_follow_decrease ?? false} onChange={(event) => updateRule(group.target_group, { only_follow_decrease: event.target.checked })} />只跟随降价</label><label><input type="checkbox" checked={draft?.rule.allow_negative_profit ?? false} onChange={(event) => updateRule(group.target_group, { allow_negative_profit: event.target.checked })} />允许负利润</label></div>
									<div className="manual-channel-table">{group.channels.map((channel) => <div key={channel.id}><span><i className={channel.status === 1 ? 'healthy' : 'disabled'} /><strong>{channel.name || `渠道 #${channel.id}`}</strong></span><code>{channel.base_origin || '未配置地址'}</code><small>#{channel.id} · 模型 {channel.models.slice(0, 5).join('、') || '未记录'}{channel.models.length > 5 ? ` 等 ${channel.models.length} 个` : ''}</small></div>)}</div>
								</div>
							</details>
						</article>;
					})}
					{visibleGroups.length === 0 && <div className="mapping-empty-filter">当前筛选下没有本站渠道分组。</div>}
				</div>
			</Section>

			<div className="mapping-submit-bar">
				<div><Save size={19} /><span><strong>已选择 {selected.size} 个本站分组</strong><small>整批验证、整批提交；任一项冲突或过期都会全部回滚。保存规则本身不会立即修改本站倍率。</small></span></div>
				<Button onClick={() => void saveSelected()} disabled={selected.size === 0 || saving}>{saving ? <LoaderCircle className="spin" size={16} /> : <Save size={16} />}{saving ? '正在复验并保存' : `保存 ${selected.size} 条映射`}</Button>
			</div>
		</>}
	</>;
}
