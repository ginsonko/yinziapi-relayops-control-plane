import { useMemo, useState } from 'react';
import {
	AlertTriangle,
	Check,
	CheckCircle2,
	GitBranch,
	LoaderCircle,
	RefreshCw,
	Save,
	Search,
	Workflow,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type {
	ManualMappingDraftPreview,
	ManualMappingSourceOption,
	ManualMappingTargetChannel,
	ManualMappingTargetGroup,
	MirrorDraftBatchResult,
	MirrorPolicy,
	MirrorPolicyDocument,
	TargetSite,
} from '../api/types';
import { Button, EmptyState, Section, StatusBadge } from '../components/ui';

interface MirrorDraft {
	candidateId: string;
	channelId: number;
	executionMode: 'auto' | 'approval';
}

interface ManualMappingDraftsProps {
	targets: TargetSite[];
	targetId: string;
	onTargetChange: (targetId: string) => void;
	onReload: () => Promise<unknown>;
	onNotify: (message: string, tone?: 'success' | 'info' | 'error') => void;
	initialError?: string;
}

function compatibleChannels(group: ManualMappingTargetGroup) {
	return group.channels.filter((channel) => channel.status === 1);
}

function optionForDraft(group: ManualMappingTargetGroup, draft?: MirrorDraft) {
	return group.options.find((option) => option.candidate_id === draft?.candidateId);
}

function policiesForGroup(policies: MirrorPolicy[], targetSiteID: number, targetGroup: string) {
	const grouped = new Map<string, MirrorPolicy>();
	for (const policy of policies) {
		if (policy.kind !== 'complete_mirror' || policy.legacy_policy_id != null || policy.target_site_id !== targetSiteID ||
			policy.target_group.trim().toLocaleLowerCase() !== targetGroup.trim().toLocaleLowerCase()) continue;
		const key = `${policy.supplier_id}\u0000${policy.source_group.trim().toLocaleLowerCase()}`;
		const current = grouped.get(key);
		if (!current || (policy.enabled && !current.enabled) ||
			(policy.enabled === current.enabled && policy.policy_version > current.policy_version)) {
			grouped.set(key, policy);
		}
	}
	return [...grouped.values()];
}

function matchingPolicy(policies: MirrorPolicy[], targetSiteID: number, targetGroup: string,
	option?: ManualMappingSourceOption) {
	if (!option) return undefined;
	return policiesForGroup(policies, targetSiteID, targetGroup).find((policy) =>
		policy.supplier_id === option.supplier_id &&
		policy.source_group.trim().toLocaleLowerCase() === option.source_group.trim().toLocaleLowerCase());
}

function defaultChannel(group: ManualMappingTargetGroup, option?: ManualMappingSourceOption) {
	const compatible = compatibleChannels(group);
	const matched = compatible.find((channel) => option?.matched_channel_ids?.includes(channel.id));
	return matched?.id ?? compatible[0]?.id ?? 0;
}

function initialDrafts(preview: ManualMappingDraftPreview, policies: MirrorPolicy[]) {
	const drafts: Record<string, MirrorDraft> = {};
	for (const group of preview.groups) {
		const native = policiesForGroup(policies, preview.target_site_id, group.target_group);
		const current = native.length === 1 ? native[0] : undefined;
		let option = current ? group.options.find((candidate) => candidate.supplier_id === current.supplier_id &&
			candidate.source_group.trim().toLocaleLowerCase() === current.source_group.trim().toLocaleLowerCase()) : undefined;
		option ??= group.options.find((candidate) => candidate.candidate_id === group.recommended_option_id) ?? group.options[0];
		drafts[group.target_group] = {
			candidateId: option?.candidate_id ?? '',
			channelId: defaultChannel(group, option),
			executionMode: current?.execution_mode === 'approval' ? 'approval' : 'auto',
		};
	}
	return drafts;
}

function newPolicyDocument(option: ManualMappingSourceOption, targetSiteID: number, targetGroup: string,
	draft: MirrorDraft): MirrorPolicyDocument {
	return {
		policy_uuid: '', policy_version: 1, schema_version: 3,
		supplier_id: option.supplier_id, source_group: option.source_group, source_model_pattern: '*',
		target_site_id: targetSiteID, target_group: targetGroup, enabled: false,
		excluded_models: [],
		kind: 'complete_mirror', price_anchor: 'source_base', sync_group_ratio: false,
		group_ratio_rule: { type: 'same' },
		sync_model_prices: true, model_price_rule: { type: 'same' }, sync_billing_mode: true,
		sync_model_additions: true, sync_model_removals: true, model_scope_mode: 'mirror_group',
		empty_group_action: 'remove_managed_models', execution_mode: draft.executionMode, rounding_step: '0.000001',
		only_follow_increase: false, only_follow_decrease: false,
	};
}

function editablePolicy(policy: MirrorPolicy, draft: MirrorDraft): MirrorPolicyDocument {
	const { legacy_policy_id: _legacyPolicyID, ...document } = policy;
	return {
		...document,
		enabled: true,
		sync_group_ratio: false,
		group_ratio_rule: { type: 'same' },
		execution_mode: draft.executionMode,
	};
}

function adoptionPolicy(policy: MirrorPolicy, draft: MirrorDraft): MirrorPolicyDocument {
	return { ...editablePolicy(policy, draft), policy_version: 1, enabled: false };
}

function policyRuleMatches(policy: MirrorPolicy, draft: MirrorDraft) {
	return policy.enabled && policy.execution_mode === draft.executionMode && !policy.sync_group_ratio;
}

export function ManualMappingDrafts(props: ManualMappingDraftsProps) {
	const [preview, setPreview] = useState<ManualMappingDraftPreview | null>(null);
	const [policies, setPolicies] = useState<MirrorPolicy[]>([]);
	const [drafts, setDrafts] = useState<Record<string, MirrorDraft>>({});
	const [selected, setSelected] = useState<Set<string>>(new Set());
	const [query, setQuery] = useState('');
	const [loading, setLoading] = useState(false);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState(props.initialError ?? '');
	const [rowMessages, setRowMessages] = useState<Record<string, { tone: 'success' | 'error'; text: string }>>({});
	const rememberPolicy = (policy: MirrorPolicy) => setPolicies((current) => [
		...current.filter((item) => item.policy_uuid !== policy.policy_uuid), policy,
	]);

	const runPreview = async (preserveMessages = false) => {
		if (!props.targetId) return;
		setLoading(true);
		setError('');
		if (!preserveMessages) setRowMessages({});
		try {
			const [mappingResponse, policyResponse] = await Promise.all([
				api.previewManualMappingDrafts({ target_site_id: Number(props.targetId) }),
				api.listMirrorPolicies(),
			]);
			setPreview(mappingResponse.data);
			setPolicies(policyResponse.data);
			setDrafts(initialDrafts(mappingResponse.data, policyResponse.data));
			setSelected(new Set());
		} catch (cause) {
			setError(getErrorMessage(cause));
		} finally {
			setLoading(false);
		}
	};

	const changeTarget = (value: string) => {
		props.onTargetChange(value);
		setPreview(null);
		setPolicies([]);
		setDrafts({});
		setSelected(new Set());
		setError('');
		setRowMessages({});
	};

	const visibleGroups = useMemo(() => {
		const normalized = query.trim().toLocaleLowerCase();
		return preview?.groups.filter((group) => !normalized || [group.target_group,
			...group.channels.flatMap((channel) => [channel.name, channel.base_origin ?? '']),
			...group.options.flatMap((option) => [option.supplier_name, option.source_group_name, option.source_group]),
		].join('\n').toLocaleLowerCase().includes(normalized)) ?? [];
	}, [preview, query]);

	const updateDraft = (targetGroup: string, patch: Partial<MirrorDraft>) => {
		setDrafts((current) => ({ ...current, [targetGroup]: { ...current[targetGroup], ...patch } }));
	};

	const chooseOption = (group: ManualMappingTargetGroup, option: ManualMappingSourceOption) => {
		setSelected((current) => { const next = new Set(current); next.delete(group.target_group); return next; });
		setRowMessages((current) => { const next = { ...current }; delete next[group.target_group]; return next; });
		updateDraft(group.target_group, {
			candidateId: option.candidate_id,
			channelId: defaultChannel(group, option),
		});
	};

	const selectAllUsable = () => {
		if (!preview) return;
		setError('');
		const nextSelected = new Set<string>();
		const messages: Record<string, { tone: 'success' | 'error'; text: string }> = {};
		for (const group of preview.groups) {
			const draft = drafts[group.target_group];
			const option = optionForDraft(group, draft);
			const groupPolicies = policiesForGroup(policies, Number(props.targetId), group.target_group);
			if (!draft?.channelId || !option) messages[group.target_group] = { tone: 'error', text: '请选择上游分组和本站渠道' };
			else if (groupPolicies.length > 1) messages[group.target_group] = { tone: 'error', text: '该本站分组存在多条镜像关系，请先处理冲突关系' };
			else if (groupPolicies.length === 1 && !matchingPolicy(policies, Number(props.targetId), group.target_group, option)) {
				messages[group.target_group] = { tone: 'error', text: '该本站分组已绑定另一个上游分组' };
			} else nextSelected.add(group.target_group);
		}
		setSelected(nextSelected);
		setRowMessages(messages);
		props.onNotify(`已选择 ${nextSelected.size} 个可配置分组${Object.keys(messages).length ? `，其余 ${Object.keys(messages).length} 个原因已显示在对应行` : ''}`,
			nextSelected.size ? 'success' : 'info');
	};

	const toggleGroup = (group: ManualMappingTargetGroup) => {
		const draft = drafts[group.target_group];
		if (!draft?.candidateId || !draft.channelId || policiesForGroup(policies, Number(props.targetId), group.target_group).length > 1) return;
		setRowMessages((current) => { const next = { ...current }; delete next[group.target_group]; return next; });
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
			if (!option || !draft.channelId) {
				setError(`${group.target_group} 需要选择上游分组和本站渠道。`);
				return;
			}
			const groupPolicies = policiesForGroup(policies, preview.target_site_id, group.target_group);
			if (groupPolicies.length === 1 && !matchingPolicy(policies, preview.target_site_id, group.target_group, option)) {
				setError(`${group.target_group} 已绑定另一上游；请先在“分组镜像”删除旧关系，再创建新关系。`);
				return;
			}
		}

		setSaving(true);
		setError('');
		try {
			let created = 0;
			let updated = 0;
			let unchanged = 0;
			let failed = 0;
			const batchResults: MirrorDraftBatchResult[] = [];
			const messages: Record<string, { tone: 'success' | 'error'; text: string }> = {};
			for (const group of groups) {
				const draft = drafts[group.target_group];
				const option = optionForDraft(group, draft)!;
				let selectedChannelID = draft.channelId;
				let channelName = group.channels.find((channel) => channel.id === selectedChannelID)?.name || `渠道 #${selectedChannelID}`;
				let committedStatus: MirrorDraftBatchResult['status'] | null = null;
				let committedLabel = '';
				try {
					let policy = matchingPolicy(policies, preview.target_site_id, group.target_group, option);
					let result = '已存在';
					let status: MirrorDraftBatchResult['status'] = 'unchanged';
					const adopt = async (document: MirrorPolicyDocument) => {
						const candidates = [selectedChannelID, ...compatibleChannels(group).map((channel) => channel.id)
							.filter((channelID) => channelID !== selectedChannelID)];
						let adoption;
						let lastReason = '当前没有可用于该分组的本站渠道';
						for (const channelID of candidates) {
							const candidate = await api.previewMirrorAdoption({ channel_id: channelID,
								policy: document });
							const blocking = candidate.data.service_gates?.filter((gate) => gate.blocks_apply) ?? [];
							if (candidate.data.executable || candidate.data.already_adopted) {
								adoption = candidate;
								selectedChannelID = channelID;
								break;
							}
							lastReason = blocking[0]?.message || '当前不能创建镜像';
							if (!blocking.length || !blocking.every((gate) => gate.code === 'channel_owned_elsewhere' || gate.code.startsWith('membership_owned_elsewhere:'))) break;
						}
						if (!adoption) throw new Error(lastReason);
						channelName = group.channels.find((channel) => channel.id === selectedChannelID)?.name || `渠道 #${selectedChannelID}`;
						return (await api.applyMirrorAdoption(adoption.data.id, true)).data.policy;
					};
					if (!policy) {
						policy = await adopt(newPolicyDocument(option, preview.target_site_id, group.target_group, draft));
						status = 'created'; result = '已新建'; committedLabel = result;
						committedStatus = status; rememberPolicy(policy);
					} else if (!policy.enabled) {
						if (policy.execution_mode !== draft.executionMode || policy.sync_group_ratio) {
							policy = (await api.updateMirrorPolicy({ ...editablePolicy(policy, draft), enabled: false })).data;
							rememberPolicy(policy);
							committedStatus = 'updated'; committedLabel = '已更新草案';
						}
						policy = await adopt(adoptionPolicy(policy, draft));
						status = 'updated'; result = '已续接并启用'; committedLabel = result;
						committedStatus = status; rememberPolicy(policy);
					} else if (!policyRuleMatches(policy, draft)) {
						policy = (await api.updateMirrorPolicy(editablePolicy(policy, draft))).data;
						status = 'updated'; result = '已更新'; committedLabel = result;
						committedStatus = status; rememberPolicy(policy);
					}
					const mirrorPreview = (await api.previewMirrorPolicy(policy.policy_uuid)).data;
					const noChange = Boolean((mirrorPreview.plan as { no_change?: boolean }).no_change);
					if (!noChange) {
						if (!mirrorPreview.executable) throw new Error(mirrorPreview.service_gates?.find((gate) => gate.blocks_apply)?.message || '当前不能同步');
						await api.applyMirrorPreview(mirrorPreview.id, true);
					}
					if (status === 'created') created += 1;
					else if (status === 'updated') updated += 1;
					else unchanged += 1;
					batchResults.push({ target_group: group.target_group, supplier: option.supplier_name,
						source_group: option.source_group_name || option.source_group, channel: channelName, status });
					messages[group.target_group] = { tone: 'success', text: `${result}并完成同步` };
				} catch (cause) {
					const reason = getErrorMessage(cause);
					if (committedStatus) {
						if (committedStatus === 'created') created += 1;
						else if (committedStatus === 'updated') updated += 1;
						else unchanged += 1;
						batchResults.push({ target_group: group.target_group, supplier: option.supplier_name,
							source_group: option.source_group_name || option.source_group, channel: channelName,
							status: committedStatus, reason: `首次同步待继续：${reason}` });
						messages[group.target_group] = { tone: 'error', text: `${committedLabel || '镜像关系已保存'}，首次同步待继续：${reason}` };
					} else {
						failed += 1;
						batchResults.push({ target_group: group.target_group, supplier: option.supplier_name,
							source_group: option.source_group_name || option.source_group, channel: channelName, status: 'failed', reason });
						messages[group.target_group] = { tone: 'error', text: reason };
					}
				}
			}
			setRowMessages(messages);
			try {
				await api.recordMirrorDraftBatch({ target_site_id: preview.target_site_id, created, updated, unchanged, failed,
					results: batchResults });
			} catch {
				// The row results remain visible even if the optional activity record cannot be saved.
			}
			props.onNotify(`分组镜像处理完成：新建 ${created}、更新 ${updated}、无需变更 ${unchanged}${failed ? `、失败 ${failed}` : ''}`, failed ? 'info' : 'success');
			await props.onReload();
			await runPreview(true);
		} catch (cause) {
			const message = getErrorMessage(cause);
			setError(message);
			props.onNotify(message, 'error');
		} finally {
			setSaving(false);
		}
	};

	return <>
		<Section title="本站分组" description="读取本站已有渠道，然后为每个分组选择对应的上游分组。" action={<Button onClick={() => void runPreview()} disabled={!props.targetId || loading}>{loading ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}{loading ? '正在读取' : preview ? '刷新' : '读取本站分组'}</Button>}>
			<div className="manual-mapping-controls"><label><span>本站</span><select value={props.targetId} onChange={(event) => changeTarget(event.target.value)}><option value="">请选择</option>{props.targets.map((target) => <option key={target.id} value={target.id}>{target.name}</option>)}</select></label></div>
			{props.targets.length === 0 && <div className="mapping-inline-warning"><AlertTriangle size={17} /><span>还没有可用的本站 NewAPI，请先在“站点接入”完成连接。</span></div>}
			{error && <div className="mapping-inline-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
		</Section>

		{!preview && !loading && props.targets.length > 0 && <EmptyState icon={<GitBranch size={25} />} title="尚未读取本站分组" description="点击“读取本站分组”开始配置。" />}

		{preview && <Section title="分组对应关系" description="镜像同步模型、真实基础价格和计费方式，不修改本站分组倍率。" action={<div className="table-actions"><Button variant="secondary" onClick={selectAllUsable}>全选可用</Button><Button variant="ghost" onClick={() => setSelected(new Set())}>清空</Button><label className="manual-mapping-search"><Search size={14} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索分组或上游" /></label></div>}>
			<div className="manual-mapping-list">{visibleGroups.map((group) => {
				const draft = drafts[group.target_group];
				const option = optionForDraft(group, draft);
				const sites = new Map<number, string>();
				for (const candidate of group.options) sites.set(candidate.supplier_id, candidate.supplier_name);
				const siteOptions = group.options.filter((candidate) => candidate.supplier_id === option?.supplier_id);
				const channels = compatibleChannels(group);
				const nativePolicies = policiesForGroup(policies, preview.target_site_id, group.target_group);
				const isSelected = selected.has(group.target_group);
				const conflicted = nativePolicies.length > 1;
				const existing = nativePolicies.length === 1;
				return <article className={`manual-mapping-row ${isSelected ? 'is-selected' : ''}`} key={group.target_group}>
					<button type="button" className="mapping-check" disabled={conflicted || !draft?.candidateId || !draft.channelId} onClick={() => toggleGroup(group)} aria-label={isSelected ? '取消选择' : '选择'}>{isSelected && <Check size={16} />}</button>
					<div className="manual-target-group"><StatusBadge status={conflicted ? 'error' : existing ? 'healthy' : 'unknown'} label={conflicted ? '关系冲突' : existing ? '已镜像' : '待配置'} /><strong>{group.target_group}</strong><small>本站倍率 {group.target_ratio || '待读取'} · {group.models.length} 个模型</small></div>
					<div className="manual-source-pickers">
						<label><span>上游站点</span><select value={option?.supplier_id ?? ''} disabled={conflicted} onChange={(event) => { const next = group.options.find((candidate) => candidate.supplier_id === Number(event.target.value)); if (next) chooseOption(group, next); }}><option value="">请选择</option>{[...sites].map(([id, name]) => <option key={id} value={id}>{name}</option>)}</select></label>
						<label><span>上游分组</span><select value={option?.candidate_id ?? ''} disabled={conflicted} onChange={(event) => { const next = group.options.find((candidate) => candidate.candidate_id === event.target.value); if (next) chooseOption(group, next); }}><option value="">请选择</option>{siteOptions.map((candidate) => <option key={candidate.candidate_id} value={candidate.candidate_id}>{candidate.source_group_name || candidate.source_group}</option>)}</select></label>
						<label><span>本站渠道</span><select value={draft?.channelId ?? 0} disabled={conflicted || channels.length === 0} onChange={(event) => { setSelected((current) => { const next = new Set(current); next.delete(group.target_group); return next; }); setRowMessages((current) => { const next = { ...current }; delete next[group.target_group]; return next; }); updateDraft(group.target_group, { channelId: Number(event.target.value) }); }}><option value={0}>请选择</option>{channels.map((channel: ManualMappingTargetChannel) => <option key={channel.id} value={channel.id}>{channel.name || `渠道 #${channel.id}`}</option>)}</select></label>
					</div>
					<div className="manual-rule-main"><label><span>执行方式</span><select value={draft?.executionMode ?? 'auto'} disabled={conflicted} onChange={(event) => updateDraft(group.target_group, { executionMode: event.target.value as 'auto' | 'approval' })}><option value="auto">自动同步</option><option value="approval">手动检查</option></select></label><small>倍率继续由“价格映射”独立控制，二者不会重复写入。</small></div>
					<div className="manual-option-evidence"><Workflow size={15} /><small>{channels.length ? `完整同步 ${group.models.length} 个模型、价格、计费方式和后续增删；模型不等时首次同步自动收敛` : '该分组没有已启用的本站渠道'}</small></div>
					{conflicted && <div className="manual-row-warning"><AlertTriangle size={14} />同一本站分组存在多条镜像关系，请在“分组镜像”中删除多余关系。</div>}
					{rowMessages[group.target_group] && <div className={rowMessages[group.target_group].tone === 'error' ? 'manual-row-warning' : 'manual-row-success'}>{rowMessages[group.target_group].tone === 'error' ? <AlertTriangle size={14} /> : <CheckCircle2 size={14} />}{rowMessages[group.target_group].text}</div>}
				</article>;
			})}{visibleGroups.length === 0 && <div className="mapping-empty-filter">没有匹配的本站分组。</div>}</div>
		</Section>}

		{preview && <div className="mapping-submit-bar"><div><CheckCircle2 size={19} /><span><strong>已选择 {selected.size} 个分组</strong><small>保存后立即完成第一次同步，之后随上游采集自动更新。</small></span></div><Button onClick={() => void saveSelected()} disabled={selected.size === 0 || saving}>{saving ? <LoaderCircle className="spin" size={16} /> : <Save size={16} />}{saving ? '正在保存并同步' : `保存并同步 ${selected.size} 条`}</Button></div>}
	</>;
}
