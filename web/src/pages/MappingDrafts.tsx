import { useEffect, useMemo, useState } from 'react';
import {
	AlertTriangle,
	Check,
	CheckCircle2,
	ChevronDown,
	CircleHelp,
	FileCheck2,
	GitCompareArrows,
	LoaderCircle,
	LockKeyhole,
	ListTree,
	RefreshCw,
	ShieldCheck,
	Sparkles,
	Workflow,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { MappingDraftCandidate, MappingDraftPreview, TargetSite } from '../api/types';
import { Button, EmptyState, PageHeader, Section } from '../components/ui';
import { preferredEnabledTarget } from '../data/targets';
import { ManualMappingDrafts as GroupMirrorDrafts } from './ManualMappingDrafts';
import { PricingManualMappingDrafts } from './PricingManualMappingDrafts';
import './MappingDrafts.css';

type Filter = 'all' | MappingDraftCandidate['status'];
type Selection = { targetGroup: string };
type MappingMode = 'automatic' | 'manual';
type DraftKind = 'pricing' | 'mirror';

const statusCopy: Record<MappingDraftCandidate['status'], { label: string; detail: string }> = {
	prefilled: { label: '可直接落稿', detail: '证据充分且映射唯一，已按当前价差预填' },
	needs_confirmation: { label: '需要确认', detail: '证据不足或存在相近候选，需要人工选择目标分组' },
	already_mapped: { label: '已有规则', detail: '现有规则拥有该映射，无需重复创建' },
	conflict: { label: '存在冲突', detail: '发现多个既有所有者，需要先整理现有规则' },
};

const reasonCopy: Record<string, string> = {
	score_below_threshold: '综合证据分不足',
	model_overlap_below_threshold: '模型重叠不足',
	no_strong_identity_evidence: '缺少同源渠道或同名分组证据',
	source_or_target_ratio_unknown: '一侧倍率尚未采集',
	source_snapshot_stale: '上游目录快照已过期',
	equivalent_cost_unresolved: '逐模型等效成本无法核对',
	equivalent_cost_coverage_below_threshold: '逐模型价格维度覆盖不足 80%',
	target_group_already_owned: '目标分组已有定价规则',
	source_match_is_ambiguous: '前两名候选过于接近',
	target_group_has_ambiguous_source_competition: '多个上游同时争用该目标分组',
	target_group_reserved_by_stronger_candidate: '该目标分组已分配给证据更强的候选',
	existing_mapping_is_authoritative: '沿用管理员已确认的现有映射',
	source_group_has_multiple_existing_mappings: '同一上游分组存在多条既有映射',
	no_target_group_options: '本站暂无可比较目标分组',
};

function percent(value?: string) {
	const parsed = Number(value ?? 0);
	return Number.isFinite(parsed) ? `${Math.round(parsed * 100)}%` : '待确认';
}

function scorePoints(value?: string) {
	const parsed = Number(value ?? 0);
	if (!Number.isFinite(parsed)) return '待确认';
	const points = parsed * 100;
	return points % 1 === 0 ? points.toFixed(0) : points.toFixed(1);
}

const factorCopy: Record<string, string> = {
	origin: '同源渠道',
	name: '名称相似',
	models: '模型交集',
	families: '模型家族',
	ratio: '倍率接近',
};

function factorEvidence(key: string, raw: string) {
	if (key === 'origin') return `本站同源渠道 ${raw.split('=').at(-1) ?? '0'} 条`;
	if (key === 'name') return raw.endsWith('true') ? '归一化名称完全相同' : '按归一化名称相似度计分';
	if (key === 'models') return raw.replace('intersection=', '交集 ').replace(',union=', ' / 并集 ');
	if (key === 'families') return raw.replace('source=', '上游 ').replace(',target=', ' / 本站 ');
	if (key === 'ratio') return raw.endsWith('true') ? '双方倍率均已采集' : '至少一侧倍率未知';
	return raw;
}

function formatTime(value?: string) {
	if (!value) return '未记录';
	const parsed = new Date(value);
	return Number.isNaN(parsed.getTime()) ? '未记录' : parsed.toLocaleString('zh-CN', { hour12: false });
}

function sourceRatioEvidence(source: { source_ratio?: string; raw_source_ratio?: string; recharge_value_factor?: string }) {
	const effective = source.source_ratio || '未知';
	const raw = source.raw_source_ratio || effective;
	const factor = source.recharge_value_factor || '1';
	return raw !== effective || factor !== '1'
		? `有效倍率 ${effective} · 原始 ${raw} × 现金折算 ${factor}`
		: `倍率 ${effective}`;
}

export function MappingDraftsPage({ onReload, onNotify, onOpenMirror }: {
	onReload: () => Promise<unknown>;
	onNotify: (message: string, tone?: 'success' | 'info' | 'error') => void;
	onOpenMirror?: () => void;
}) {
	const [targets, setTargets] = useState<TargetSite[]>([]);
	const [targetId, setTargetId] = useState('');
	const [preview, setPreview] = useState<MappingDraftPreview | null>(null);
	const [loadingTargets, setLoadingTargets] = useState(true);
	const [analyzing, setAnalyzing] = useState(false);
	const [materializing, setMaterializing] = useState(false);
	const [error, setError] = useState('');
	const [filter, setFilter] = useState<Filter>('all');
	const [selections, setSelections] = useState<Record<string, Selection>>({});
	const [minimumScore, setMinimumScore] = useState('0.78');
	const [minimumModels, setMinimumModels] = useState('0.50');
	const [minimumMargin, setMinimumMargin] = useState('0.08');
	const [maximumOptions, setMaximumOptions] = useState('5');
	const [mode, setMode] = useState<MappingMode>('automatic');
	const [draftKind, setDraftKind] = useState<DraftKind>('pricing');

	useEffect(() => {
		let active = true;
		void api.listTargets().then((response) => {
			if (!active) return;
			const enabled = response.data.filter((item) => item.enabled);
			setTargets(enabled);
			setTargetId((current) => current || String(preferredEnabledTarget(enabled)?.id ?? ''));
		}).catch((cause) => active && setError(getErrorMessage(cause))).finally(() => active && setLoadingTargets(false));
		return () => { active = false; };
	}, []);

	const runPreview = async () => {
		if (!targetId) return;
		setAnalyzing(true);
		setError('');
		try {
			const response = await api.previewMappingDrafts({
				target_site_id: Number(targetId),
				minimum_score: minimumScore,
				minimum_model_jaccard: minimumModels,
				minimum_score_margin: minimumMargin,
				maximum_options: Number(maximumOptions),
			});
			setPreview(response.data);
			const defaults: Record<string, Selection> = {};
			for (const candidate of response.data.candidates) {
				if (candidate.status === 'prefilled' && candidate.proposed_target_group) {
					defaults[candidate.id] = { targetGroup: candidate.proposed_target_group };
				}
			}
			setSelections(defaults);
			setFilter('all');
		} catch (cause) {
			setError(getErrorMessage(cause));
		} finally {
			setAnalyzing(false);
		}
	};

	const visibleCandidates = useMemo(() => preview?.candidates.filter((item) => filter === 'all' || item.status === filter) ?? [], [preview, filter]);
	const selectedCandidates = useMemo(() => preview?.candidates.filter((item) => selections[item.id]) ?? [], [preview, selections]);

	const toggleCandidate = (candidate: MappingDraftCandidate) => {
		if (candidate.status === 'already_mapped' || candidate.status === 'conflict' || candidate.options.length === 0) return;
		setSelections((current) => {
			if (current[candidate.id]) {
				const next = { ...current };
				delete next[candidate.id];
				return next;
			}
			return { ...current, [candidate.id]: { targetGroup: candidate.proposed_target_group || candidate.options[0].target_group } };
		});
	};

	const chooseTarget = (candidate: MappingDraftCandidate, targetGroup: string) => {
		setSelections((current) => ({ ...current, [candidate.id]: { targetGroup } }));
	};

	const materialize = async () => {
		if (!preview || selectedCandidates.length === 0) return;
		setMaterializing(true);
		setError('');
		try {
			const response = await api.materializeMappingDrafts({
				target_site_id: preview.target_site_id,
				target_evidence_hash: preview.target_evidence_hash,
				selections: selectedCandidates.map((candidate) => ({
					candidate_id: candidate.id,
					supplier_id: candidate.supplier_id,
					source_group: candidate.source_group,
					source_snapshot_hash: candidate.source_snapshot_hash,
					target_group: selections[candidate.id].targetGroup,
					confirm_low_confidence: candidate.status !== 'prefilled' || selections[candidate.id].targetGroup !== candidate.proposed_target_group,
				})),
			});
			const created = response.data.policies.filter((item) => item.created).length;
			const reused = response.data.policies.length - created;
			onNotify(`已创建 ${created} 条禁用审批草案${reused ? `，${reused} 条沿用现有规则` : ''}`, 'success');
			await onReload();
			await runPreview();
		} catch (cause) {
			const message = getErrorMessage(cause);
			setError(message);
			onNotify(message, 'error');
		} finally {
			setMaterializing(false);
		}
	};

	if (loadingTargets) return <div className="mapping-loading"><LoaderCircle className="spin" size={20} />正在读取本站目标...</div>;

	return <>
		<PageHeader eyebrow="Mapping drafts" title="映射草案"
			description={draftKind === 'mirror'
				? '批量建立本站分组与上游分组的镜像关系，同步模型、模型价格和计费方式，不修改分组倍率。'
				: mode === 'automatic' ? '让系统按来源、模型、名称和价格证据寻找高置信价格映射；综合匹配分用于排序，不代表正确概率。' : '以本站已有渠道为主表，按渠道名称和地址线索手动选择上游与分组，并配置价格规则。'}
			actions={draftKind === 'mirror'
				? onOpenMirror && <Button variant="secondary" onClick={onOpenMirror}><Workflow size={16} />管理镜像关系</Button>
				: mode === 'automatic' ? <Button onClick={() => void runPreview()} disabled={!targetId || analyzing}>{analyzing ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}{analyzing ? '正在分析' : preview ? '重新分析' : '生成草案'}</Button> : undefined} />

		<div className="mapping-draft-kind-switch segmented" role="tablist" aria-label="草案类型">
			<button type="button" role="tab" aria-selected={draftKind === 'pricing'} className={draftKind === 'pricing' ? 'active' : ''} onClick={() => setDraftKind('pricing')}><GitCompareArrows size={15} />价格映射草案</button>
			<button type="button" role="tab" aria-selected={draftKind === 'mirror'} className={draftKind === 'mirror' ? 'active' : ''} onClick={() => setDraftKind('mirror')}><Workflow size={15} />分组镜像草案</button>
		</div>

		{draftKind === 'pricing' ? <>

		<div className="mapping-mode-switch segmented" role="tablist" aria-label="映射草案生成方式">
			<button type="button" role="tab" aria-selected={mode === 'automatic'} className={mode === 'automatic' ? 'active' : ''} onClick={() => setMode('automatic')}><Sparkles size={15} />自动证据匹配</button>
			<button type="button" role="tab" aria-selected={mode === 'manual'} className={mode === 'manual' ? 'active' : ''} onClick={() => setMode('manual')}><ListTree size={15} />按本站已有渠道配置</button>
		</div>

		{mode === 'automatic' ? <>

		<div className="mapping-flow" aria-label="映射草案工作流">
			<div><GitCompareArrows size={18} /><span>只读比对</span><small>目录、渠道与模型证据</small></div>
			<i />
			<div><CircleHelp size={18} /><span>管理员确认</span><small>低置信项不会代选</small></div>
			<i />
			<div><LockKeyhole size={18} /><span>禁用草案</span><small>不改价、不自动执行</small></div>
		</div>

		<Section title="分析范围" description="默认使用全部已启用上游；高级阈值只能收紧，不能低于生产安全线。">
			<div className="mapping-controls">
				<label><span>本站目标</span><select value={targetId} onChange={(event) => { setTargetId(event.target.value); setPreview(null); setSelections({}); }}><option value="">请选择</option>{targets.map((target) => <option key={target.id} value={target.id}>{target.name}</option>)}</select></label>
				<details>
					<summary>高级匹配阈值 <ChevronDown size={15} /></summary>
					<div className="mapping-thresholds">
						<label><span>最低综合匹配分（0–1）</span><input type="number" min="0.78" max="1" step="0.01" value={minimumScore} onChange={(event) => setMinimumScore(event.target.value)} /></label>
						<label><span>最低模型重叠</span><input type="number" min="0.5" max="1" step="0.01" value={minimumModels} onChange={(event) => setMinimumModels(event.target.value)} /></label>
						<label><span>最低领先间隔</span><input type="number" min="0.08" max="1" step="0.01" value={minimumMargin} onChange={(event) => setMinimumMargin(event.target.value)} /></label>
						<label><span>每组候选数</span><input type="number" min="1" max="20" step="1" value={maximumOptions} onChange={(event) => setMaximumOptions(event.target.value)} /></label>
					</div>
				</details>
			</div>
			{targets.length === 0 && <div className="mapping-inline-warning"><AlertTriangle size={17} /><span>尚未连接可用的本站 NewAPI 目标，请先在“站点接入”完成连接。</span></div>}
			{error && <div className="mapping-inline-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
		</Section>

		{!preview && !analyzing && targets.length > 0 && <EmptyState icon={<GitCompareArrows size={25} />} title="尚未生成映射草案" description="选择本站目标后点击“生成草案”。RelayOps 会实时读取目标，但不会向目标发送任何写请求。" />}

		{preview && <>
			<div className="mapping-summary">
				<div><strong>{preview.summary.prefilled}</strong><span>可直接落稿</span></div>
				<div><strong>{preview.summary.needs_confirmation}</strong><span>需要确认</span></div>
				<div><strong>{preview.summary.already_mapped}</strong><span>已有规则</span></div>
				<div><strong>{preview.summary.conflicts}</strong><span>冲突</span></div>
				<div><strong>{preview.summary.suppliers_usable}/{preview.summary.suppliers_requested}</strong><span>上游目录可用</span></div>
			</div>

			<div className="mapping-score-guide"><CircleHelp size={18} /><div><strong>综合匹配分用于排序，不代表“有多少概率映射正确”</strong><span>满分 100 分：模型交集 40 分、名称相似 25 分、本站同源渠道 20 分、模型家族 10 分、倍率接近 5 分。系统还会检查现有规则归属、候选竞争、目录新鲜度和价格维度覆盖；只有证据充分且明显领先时才会自动预填。</span></div></div>

			{preview.supplier_issues.length > 0 && <div className="mapping-issues"><AlertTriangle size={17} /><div><strong>{preview.supplier_issues.length} 个上游未进入本次比对</strong>{preview.supplier_issues.map((issue) => <span key={issue.supplier_id}>{issue.supplier_name || `#${issue.supplier_id}`}：{issue.message}</span>)}</div></div>}

			<Section title="候选映射" description={`目标证据 ${preview.target_evidence_hash.slice(0, 10)} · 生成于 ${formatTime(preview.generated_at)}`} action={<div className="mapping-filters">{(['all', 'prefilled', 'needs_confirmation', 'already_mapped', 'conflict'] as Filter[]).map((value) => <button key={value} className={filter === value ? 'active' : ''} onClick={() => setFilter(value)}>{value === 'all' ? '全部' : statusCopy[value].label}</button>)}</div>}>
				<div className="mapping-list">
					{visibleCandidates.map((candidate) => {
						const selection = selections[candidate.id];
						const selectedOption = candidate.options.find((item) => item.target_group === selection?.targetGroup) ?? candidate.options[0];
						const selectable = candidate.status !== 'already_mapped' && candidate.status !== 'conflict' && candidate.options.length > 0;
						return <article className={`mapping-row mapping-${candidate.status} ${selection ? 'is-selected' : ''}`} key={candidate.id}>
							<button className="mapping-check" aria-label={selection ? '取消选择' : '选择草案'} title={selectable ? (selection ? '取消选择' : '选择草案') : statusCopy[candidate.status].detail} disabled={!selectable} onClick={() => toggleCandidate(candidate)}>{selection ? <Check size={16} /> : null}</button>
							<div className="mapping-source"><strong>{candidate.supplier_name}</strong><span>{candidate.source_group_name}</span><small>{sourceRatioEvidence(candidate)} · {candidate.source_models} 个模型 · {candidate.source_fresh ? '快照新鲜' : '快照过期'}</small></div>
							<div className="mapping-arrow"><GitCompareArrows size={18} /></div>
							<div className="mapping-target">
								<select value={selection?.targetGroup || candidate.proposed_target_group || selectedOption?.target_group || ''} disabled={!selectable} onChange={(event) => chooseTarget(candidate, event.target.value)}>{candidate.options.map((option) => <option key={option.target_group} value={option.target_group}>{option.target_group} · 倍率 {option.target_ratio || '未知'} · 综合分 {scorePoints(option.score)}/100</option>)}</select>
								{candidate.policy && <small>等效进货 {selectedOption?.equivalent_cost_ratio || candidate.source_ratio || '未知'} · 固定差 {Number(candidate.policy.value) >= 0 ? '+' : ''}{candidate.policy.value} · 审批 · 默认禁用</small>}
								{selection && candidate.status !== 'prefilled' && <small className="manual-confirm">已选择人工确认模式</small>}
							</div>
							<div className="mapping-confidence"><span className={`mapping-status status-${candidate.status}`}>{candidate.status === 'prefilled' ? <CheckCircle2 size={13} /> : candidate.status === 'conflict' ? <AlertTriangle size={13} /> : <CircleHelp size={13} />}{statusCopy[candidate.status].label}</span><strong>{scorePoints(candidate.score)} / 100</strong><small>{candidate.score_is_best ? '领先' : '落后'}最强替代项 {scorePoints(candidate.source_score_margin)} 分 · 这是证据匹配分，不是概率</small></div>
							<details className="mapping-evidence">
								<summary>查看证据 <ChevronDown size={14} /></summary>
								{selectedOption ? <div>
									<span>模型重叠 <b>{percent(selectedOption.model_jaccard)}</b></span><span>名称相似 <b>{percent(selectedOption.name_similarity)}</b></span><span>模型家族 <b>{percent(selectedOption.family_jaccard)}</b></span><span>同源渠道 <b>{selectedOption.origin_match ? `是${selectedOption.matched_channel_ids?.length ? ` · ${selectedOption.matched_channel_ids.length} 条` : ''}` : '否'}</b></span><span>等效进货倍率 <b>{selectedOption.equivalent_cost_ratio || '待核对'}</b></span><span>成本口径 <b>{selectedOption.pricing_basis === 'model_dimension_equivalent' ? `逐模型维度${selectedOption.pricing_coverage_ratio ? ` · 覆盖 ${percent(selectedOption.pricing_coverage_ratio)}` : ''}` : selectedOption.pricing_basis === 'group_ratio_fallback' ? '分组倍率回退' : '无法核对'}</b></span><span>目标竞争 <b>{candidate.target_competition.contenders} 个上游候选</b></span><span>分差含义 <b>{scorePoints(candidate.source_score_margin)} 分，不是概率差</b></span>
									<div className="mapping-factor-breakdown">{selectedOption.factors.map((factor) => <div key={factor.key}><span><b>{factorCopy[factor.key] || factor.key}</b><em>贡献 {scorePoints(factor.component)} / {scorePoints(factor.weight)} 分</em></span><small>{factorEvidence(factor.key, factor.evidence)}</small></div>)}</div>
									<p>{selectedOption.overlapping_models.slice(0, 8).join('、') || '暂无重叠模型'}{selectedOption.overlapping_models_omitted ? ` 等 ${selectedOption.overlapping_models.length + selectedOption.overlapping_models_omitted} 个` : ''}</p>
									{selectedOption.pricing_impact_error && <p>成本核对：{selectedOption.pricing_impact_error}</p>}
									{selectedOption.pricing_unknown_dimensions && selectedOption.pricing_unknown_dimensions.length > 0 && <p>未知价格维度：{selectedOption.pricing_unknown_dimensions.slice(0, 6).join('、')}{selectedOption.pricing_unknown_dimensions.length > 6 ? ' 等' : ''}</p>}
									{(candidate.reasons.length > 0 || selectedOption.ineligible_reasons.length > 0) && <ul>{Array.from(new Set([...candidate.reasons, ...selectedOption.ineligible_reasons])).map((reason) => <li key={reason}>{reasonCopy[reason] || reason}</li>)}</ul>}
								</div> : <div><p>没有可比较的本站目标分组。</p>{candidate.reasons.length > 0 && <ul>{candidate.reasons.map((reason) => <li key={reason}>{reasonCopy[reason] || reason}</li>)}</ul>}</div>}
							</details>
						</article>;
					})}
					{visibleCandidates.length === 0 && <div className="mapping-empty-filter">当前筛选下没有候选。</div>}
				</div>
			</Section>

			<div className="mapping-submit-bar">
				<div><ShieldCheck size={19} /><span><strong>已选择 {selectedCandidates.length} 条</strong><small>提交后只创建禁用审批草案；目标站点价格不会变化。</small></span></div>
				<Button onClick={() => void materialize()} disabled={selectedCandidates.length === 0 || materializing}>{materializing ? <LoaderCircle className="spin" size={16} /> : <FileCheck2 size={16} />}{materializing ? '正在复验并落稿' : `创建 ${selectedCandidates.length} 条草案`}</Button>
			</div>
		</>}
		</> : <PricingManualMappingDrafts targets={targets} targetId={targetId} onTargetChange={setTargetId} onReload={onReload} onNotify={onNotify} />}
		</> : <GroupMirrorDrafts targets={targets} targetId={targetId} onTargetChange={setTargetId} onReload={onReload} onNotify={onNotify} initialError={error} />}
	</>;
}
