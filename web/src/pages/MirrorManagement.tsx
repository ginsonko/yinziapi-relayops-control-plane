import { useEffect, useMemo, useRef, useState } from 'react';
import {
	AlertTriangle,
	LoaderCircle,
	PauseCircle,
	Pencil,
	Play,
	RefreshCw,
	Save,
	Trash2,
	Workflow,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type {
	CatalogGroup,
	CatalogSnapshot,
	GroupModelPricingSetting,
	MirrorAdoptionPreview,
	MirrorChangeSet,
	MirrorPolicy,
	MirrorPolicyDocument,
	MirrorPreview,
	RawSupplier,
	TargetChannel,
	TargetSite,
} from '../api/types';
import { Button, EmptyState, formatDateTime, Modal, PageHeader, Section, StatusBadge } from '../components/ui';

type Notify = (message: string, tone?: 'success' | 'info' | 'error') => void;

function newPolicyDocument(supplierID: number, sourceGroup: string, targetID: number, targetGroup: string): MirrorPolicyDocument {
	return {
		policy_uuid: '', policy_version: 1, schema_version: 3,
		supplier_id: supplierID, source_group: sourceGroup, source_model_pattern: '*',
		target_site_id: targetID, target_group: targetGroup, enabled: false,
		kind: 'complete_mirror', price_anchor: 'source_base',
		excluded_models: [],
		sync_group_ratio: false,
		group_ratio_rule: { type: 'same' },
		sync_model_prices: true, model_price_rule: { type: 'same' }, sync_billing_mode: true,
		sync_model_additions: true, sync_model_removals: true, model_scope_mode: 'mirror_group',
		empty_group_action: 'remove_managed_models', execution_mode: 'auto', rounding_step: '0.000001',
		only_follow_increase: false, only_follow_decrease: false,
	};
}

function editableDocument(policy: MirrorPolicy, enabled = policy.enabled, executionMode = policy.execution_mode): MirrorPolicyDocument {
	const { legacy_policy_id: _legacyPolicyID, ...document } = policy;
	return { ...document, enabled, sync_group_ratio: false, group_ratio_rule: { type: 'same' }, execution_mode: executionMode };
}

function adoptionDocument(policy: MirrorPolicy): MirrorPolicyDocument {
	return { ...editableDocument(policy, false), policy_version: 1 };
}

function needsChannelOwnership(cause: unknown) {
	const message = getErrorMessage(cause).toLocaleLowerCase();
	return message.includes('exact active channel ownership') || message.includes('one exact active channel');
}

function groupIdentity(group: CatalogGroup) {
	return group.external_id.trim() || group.name.trim();
}

function parseModelList(value: string) {
	return [...new Set(value.split(/[\n,]/).map((item) => item.trim()).filter(Boolean))];
}

function channelGroups(channel?: TargetChannel) {
	return [...new Set((channel?.group ?? '').split(',').map((item) => item.trim()).filter(Boolean))];
}

function billingLabel(setting: GroupModelPricingSetting) {
	if (setting.task_profile?.unit === 'fixed_duration') {
		return `固定时长 ${setting.task_profile.fixed_duration_seconds ?? '?'} 秒`;
	}
	return ({ fixed_price: '按次', per_call: '按次', ratio: '按 Token', token_ratio: '按 Token',
		tiered_expr: '按表达式' } as Record<string, string>)[setting.billing_mode] ?? setting.billing_mode;
}

function priceSummary(setting: GroupModelPricingSetting) {
	const values = [
		['固定', setting.model_price], ['输入', setting.model_ratio], ['输出', setting.completion_ratio],
		['缓存', setting.cache_ratio], ['图片', setting.image_ratio], ['音频', setting.audio_ratio],
	].filter((item): item is [string, string] => !!item[1]);
	if (setting.billing_expr) return setting.billing_expr;
	return values.length ? values.map(([label, value]) => `${label} ${value}`).join(' / ') : '继承全局价格';
}

export function MirrorManagementPage({ onNotify }: { onNotify: Notify }) {
	const [suppliers, setSuppliers] = useState<RawSupplier[]>([]);
	const [targets, setTargets] = useState<TargetSite[]>([]);
	const [policies, setPolicies] = useState<MirrorPolicy[]>([]);
	const [changes, setChanges] = useState<MirrorChangeSet[]>([]);
	const [snapshot, setSnapshot] = useState<CatalogSnapshot | null>(null);
	const [channels, setChannels] = useState<TargetChannel[]>([]);
	const [supplierID, setSupplierID] = useState('');
	const [sourceGroup, setSourceGroup] = useState('');
	const [targetID, setTargetID] = useState('');
	const [channelID, setChannelID] = useState('');
	const [targetGroup, setTargetGroup] = useState('');
	const [adoptionPreview, setAdoptionPreview] = useState<MirrorAdoptionPreview | null>(null);
	const [mirrorPreview, setMirrorPreview] = useState<MirrorPreview | null>(null);
	const [editingPolicy, setEditingPolicy] = useState<MirrorPolicy | null>(null);
	const [editExecutionMode, setEditExecutionMode] = useState<'auto' | 'approval'>('auto');
	const [editExcludedModels, setEditExcludedModels] = useState('');
	const [deleteCandidate, setDeleteCandidate] = useState<MirrorPolicy | null>(null);
	const [loading, setLoading] = useState(true);
	const [sourceLoading, setSourceLoading] = useState(false);
	const [targetLoading, setTargetLoading] = useState(false);
	const [busy, setBusy] = useState('');
	const [error, setError] = useState('');
	const sourceGeneration = useRef(0);
	const targetGeneration = useRef(0);
	const rememberPolicy = (policy: MirrorPolicy) => setPolicies((current) => [
		...current.filter((item) => item.policy_uuid !== policy.policy_uuid), policy,
	]);

	const loadOverview = async () => {
		setLoading(true); setError('');
		try {
			const [supplierResult, targetResult, policyResult, changeResult] = await Promise.all([
				api.listSuppliers(), api.listTargets(), api.listMirrorPolicies(), api.listMirrorChangeSets({ limit: 100 }),
			]);
			const enabledSuppliers = (supplierResult.data ?? []).filter((item) => item.enabled);
			const enabledTargets = (targetResult.data ?? []).filter((item) => item.enabled);
			setSuppliers(enabledSuppliers); setTargets(enabledTargets);
			setPolicies((policyResult.data ?? []).filter((item) => item.kind === 'complete_mirror'));
			setChanges(changeResult.data ?? []);
			setSupplierID((current) => current || String(enabledSuppliers[0]?.id ?? ''));
			setTargetID((current) => current || String(enabledTargets[0]?.id ?? ''));
		} catch (cause) { setError(getErrorMessage(cause)); } finally { setLoading(false); }
	};

	useEffect(() => { void loadOverview(); }, []);
	useEffect(() => {
		const id = Number(supplierID); const generation = ++sourceGeneration.current;
		setSnapshot(null); setSourceGroup(''); setAdoptionPreview(null); setMirrorPreview(null);
		if (!id) { setSourceLoading(false); return; }
		setSourceLoading(true);
		void api.supplierSnapshot(id).then((response) => {
			if (generation !== sourceGeneration.current) return;
			setSnapshot(response.data);
			setSourceGroup(groupIdentity(response.data.groups[0] ?? { external_id: '', name: '', model_ids: [] }));
		}).catch((cause) => generation === sourceGeneration.current && setError(getErrorMessage(cause)))
			.finally(() => generation === sourceGeneration.current && setSourceLoading(false));
	}, [supplierID]);
	useEffect(() => {
		const id = Number(targetID); const generation = ++targetGeneration.current;
		setChannels([]); setChannelID(''); setTargetGroup(''); setAdoptionPreview(null); setMirrorPreview(null);
		if (!id) { setTargetLoading(false); return; }
		setTargetLoading(true);
		void api.listTargetChannels(id).then((response) => {
			if (generation !== targetGeneration.current) return;
			setChannels(response.data);
			const first = response.data[0];
			setChannelID(String(first?.id ?? ''));
			setTargetGroup(channelGroups(first)[0] ?? '');
		}).catch((cause) => generation === targetGeneration.current && setError(getErrorMessage(cause)))
			.finally(() => generation === targetGeneration.current && setTargetLoading(false));
	}, [targetID]);

	const selectedChannel = useMemo(() => channels.find((item) => String(item.id) === channelID), [channels, channelID]);
	const sourceGroups = useMemo(() => [...(snapshot?.groups ?? [])].sort((a, b) => a.name.localeCompare(b.name, 'zh-CN')), [snapshot]);
	const contractEntries = useMemo(() => Object.entries(adoptionPreview?.contract_models ?? {}).sort(([a], [b]) => a.localeCompare(b)), [adoptionPreview]);
	const lastChange = useMemo(() => {
		const result = new Map<string, MirrorChangeSet>();
		for (const change of changes) if (!result.has(change.policy_uuid)) result.set(change.policy_uuid, change);
		return result;
	}, [changes]);
	const visiblePolicies = useMemo(() => {
		const grouped = new Map<string, MirrorPolicy>();
		for (const policy of policies) {
			const key = [policy.supplier_id, policy.source_group.trim(), policy.target_site_id, policy.target_group.trim()].join('\u0000');
			const current = grouped.get(key);
			if (!current || (policy.enabled && !current.enabled) ||
				(policy.enabled === current.enabled && policy.policy_version > current.policy_version)) {
				grouped.set(key, policy);
			}
		}
		return [...grouped.values()].sort((a, b) =>
			`${a.supplier_id}:${a.source_group}:${a.target_site_id}:${a.target_group}`.localeCompare(
			`${b.supplier_id}:${b.source_group}:${b.target_site_id}:${b.target_group}`, 'zh-CN'));
	}, [policies]);
	const canPreview = !!supplierID && !!sourceGroup && !!targetID && !!channelID && !!targetGroup;

	const previewAdoption = async () => {
		if (!canPreview) return;
		setBusy('preview'); setError(''); setMirrorPreview(null);
		try {
			const document = newPolicyDocument(Number(supplierID), sourceGroup, Number(targetID), targetGroup);
			setAdoptionPreview((await api.previewMirrorAdoption({ channel_id: Number(channelID), policy: document })).data);
		} catch (cause) { setAdoptionPreview(null); setError(getErrorMessage(cause)); }
		finally { setBusy(''); }
	};

	const adoptAndSync = async () => {
		if (!adoptionPreview) return;
		setBusy('create'); setError('');
		let adopted: MirrorPolicy | null = null;
		try {
			const adoption = await api.applyMirrorAdoption(adoptionPreview.id, true);
			adopted = adoption.data.policy;
			rememberPolicy(adopted);
			const next = (await api.previewMirrorPolicy(adoption.data.policy.policy_uuid)).data;
			setMirrorPreview(next);
			const noChange = Boolean((next.plan as { no_change?: boolean }).no_change);
			if (!noChange && !next.executable) throw new Error(next.service_gates?.find((gate) => gate.blocks_apply)?.message || '当前状态暂时不能同步');
			if (!noChange) await api.applyMirrorPreview(next.id, true);
			onNotify(noChange ? '分组镜像已创建，模型和价格已经一致。' : '分组镜像已创建并完成首次同步。');
			setAdoptionPreview(null);
			await loadOverview();
		} catch (cause) {
			const message = adopted ? `分组镜像已创建并启用，首次同步待继续：${getErrorMessage(cause)}` : getErrorMessage(cause);
			setError(message); onNotify(message, adopted ? 'info' : 'error');
		}
		finally { setBusy(''); }
	};

	const syncPolicy = async (policy: MirrorPolicy) => {
		setBusy(`sync:${policy.policy_uuid}`); setError('');
		try {
			const preview = (await api.previewMirrorPolicy(policy.policy_uuid)).data;
			setMirrorPreview(preview);
			if (!preview.executable) throw new Error(preview.service_gates?.find((gate) => gate.blocks_apply)?.message || '当前状态暂时不能同步');
			const result = await api.applyMirrorPreview(preview.id, true);
			onNotify(result.data.replayed ? '模型、价格和计费方式已经与上游一致。' : '已同步模型、价格和计费方式。');
			await loadOverview();
		} catch (cause) { setError(getErrorMessage(cause)); onNotify(getErrorMessage(cause), 'error'); }
		finally { setBusy(''); }
	};

	const togglePolicy = async (policy: MirrorPolicy) => {
		setBusy(`toggle:${policy.policy_uuid}`); setError('');
		try {
			if (policy.enabled) {
				const updated = (await api.updateMirrorPolicy(editableDocument(policy, false))).data;
				rememberPolicy(updated);
				onNotify('分组镜像已暂停。');
			} else {
				let enabled: MirrorPolicy;
				try {
					enabled = (await api.updateMirrorPolicy(editableDocument(policy, true))).data;
				} catch (cause) {
					if (!needsChannelOwnership(cause)) throw cause;
					const targetChannels = (await api.listTargetChannels(policy.target_site_id)).data.filter((channel) =>
						channel.status === 1 && channelGroups(channel).some((group) => group.toLocaleLowerCase() === policy.target_group.trim().toLocaleLowerCase()));
					if (targetChannels.length === 0) throw new Error(`本站分组 ${policy.target_group} 没有已启用渠道，无法续接镜像。`);
					if (targetChannels.length > 1) throw new Error(`本站分组 ${policy.target_group} 有 ${targetChannels.length} 条已启用渠道，请在上方选择要接管的渠道。`);
					const adoption = (await api.previewMirrorAdoption({ channel_id: targetChannels[0].id, policy: adoptionDocument(policy) })).data;
					if (!adoption.executable && !adoption.already_adopted) {
						throw new Error(adoption.service_gates?.find((gate) => gate.blocks_apply)?.message || '当前渠道无法续接镜像');
					}
					enabled = (await api.applyMirrorAdoption(adoption.id, true)).data.policy;
				}
				rememberPolicy(enabled);
				try {
					const preview = (await api.previewMirrorPolicy(enabled.policy_uuid)).data;
					setMirrorPreview(preview);
					const noChange = Boolean((preview.plan as { no_change?: boolean }).no_change);
					if (!noChange && !preview.executable) throw new Error(preview.service_gates?.find((gate) => gate.blocks_apply)?.message || '当前状态暂时不能同步');
					if (!noChange) await api.applyMirrorPreview(preview.id, true);
					onNotify(noChange ? '分组镜像已启用，当前模型和价格已经一致。' : '分组镜像已启用并完成同步。');
				} catch (cause) {
					const message = `分组镜像已启用，首次同步待继续：${getErrorMessage(cause)}`;
					setError(message); onNotify(message, 'info');
				}
			}
			await loadOverview();
		} catch (cause) { const message = getErrorMessage(cause); setError(message); onNotify(message, 'error'); }
		finally { setBusy(''); }
	};

	const startEdit = (policy: MirrorPolicy) => {
		setEditingPolicy(policy); setEditExecutionMode(policy.execution_mode === 'approval' ? 'approval' : 'auto');
		setEditExcludedModels((policy.excluded_models ?? []).join('\n'));
	};
	const saveEdit = async () => {
		if (!editingPolicy) return;
		setBusy(`edit:${editingPolicy.policy_uuid}`); setError('');
		try {
			const document = editableDocument(editingPolicy, editingPolicy.enabled, editExecutionMode);
			document.excluded_models = parseModelList(editExcludedModels);
			await api.updateMirrorPolicy(document);
			setEditingPolicy(null); onNotify('镜像执行方式已更新。'); await loadOverview();
		} catch (cause) { setError(getErrorMessage(cause)); }
		finally { setBusy(''); }
	};
	const deletePolicy = async () => {
		if (!deleteCandidate) return;
		setBusy(`delete:${deleteCandidate.policy_uuid}`); setError('');
		try {
			await api.deleteMirrorPolicy(deleteCandidate.policy_uuid, deleteCandidate.policy_version);
			setDeleteCandidate(null); onNotify('分组镜像关系已删除，真实渠道和现有价格保持不变。'); await loadOverview();
		} catch (cause) { setError(getErrorMessage(cause)); }
		finally { setBusy(''); }
	};

	if (loading) return <div className="mirror-loading"><LoaderCircle className="spin" size={22} />正在读取分组镜像</div>;
	return <>
		<PageHeader eyebrow="Group mirror" title="分组镜像" description="把一个上游分组的模型、基础价格和计费方式同步到本站分组；利润只需通过分组倍率控制。" actions={<Button variant="secondary" onClick={() => void loadOverview()}><RefreshCw size={16} />刷新</Button>} />
		{error && <div className="mirror-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}

		<Section title="新建分组镜像" description="选择一组对应关系。模型价格、计费方式以及模型新增和下架会自动保持同步。">
			<fieldset className="mirror-adoption-controls" disabled={!!busy}>
				<div className="mirror-adoption-grid">
					<label className="field"><span>上游站点</span><select value={supplierID} onChange={(event) => setSupplierID(event.target.value)}><option value="">请选择</option>{suppliers.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>
					<label className="field"><span>上游分组</span><select value={sourceGroup} disabled={sourceLoading} onChange={(event) => { setSourceGroup(event.target.value); setAdoptionPreview(null); }}><option value="">{sourceLoading ? '正在读取' : '请选择'}</option>{sourceGroups.map((item) => <option key={groupIdentity(item)} value={groupIdentity(item)}>{item.name || item.external_id} ({item.model_ids.length} 个模型)</option>)}</select></label>
					<label className="field"><span>本站</span><select value={targetID} onChange={(event) => setTargetID(event.target.value)}><option value="">请选择</option>{targets.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>
					<label className="field"><span>本站渠道</span><select value={channelID} disabled={targetLoading} onChange={(event) => { const value = event.target.value; const channel = channels.find((item) => String(item.id) === value); setChannelID(value); setTargetGroup(channelGroups(channel)[0] ?? ''); setAdoptionPreview(null); }}><option value="">{targetLoading ? '正在读取' : '请选择'}</option>{channels.map((item) => <option key={item.id} value={item.id}>{item.name || `渠道 #${item.id}`} · {item.models.length} 个模型</option>)}</select></label>
					<label className="field"><span>本站分组</span><select value={targetGroup} onChange={(event) => { setTargetGroup(event.target.value); setAdoptionPreview(null); }}><option value="">请选择</option>{channelGroups(selectedChannel).map((item) => <option key={item} value={item}>{item}</option>)}</select></label>
				</div>
				<div className="mirror-adoption-actions"><span>镜像只同步模型、真实基础价格和计费方式；分组倍率由价格映射规则单独控制。</span><Button disabled={!canPreview || !!busy} onClick={() => void previewAdoption()}>{busy === 'preview' ? <LoaderCircle className="spin" size={16} /> : <Workflow size={16} />}预览</Button></div>
			</fieldset>
		</Section>

		{adoptionPreview && <Section title="确认创建" description={`${adoptionPreview.source_model_count} 个上游模型将同步到本站分组。`}>
			<div className="mirror-proof-strip"><div><span>本站渠道</span><strong>#{adoptionPreview.channel.id} {adoptionPreview.channel.name}</strong></div><div><span>当前本站模型</span><strong>{contractEntries.length}</strong></div><div><span>上游模型</span><strong>{adoptionPreview.source_model_count}</strong></div><div><span>同名模型</span><strong>{adoptionPreview.source_target_overlap}</strong></div></div>
			{(adoptionPreview.service_gates ?? []).filter((gate) => gate.blocks_apply).map((gate) => <div className="mirror-error" key={gate.code}><AlertTriangle size={15} /><span>{gate.message}</span></div>)}
			<div className="table-wrap"><table><thead><tr><th>模型</th><th>计费方式</th><th>当前本站基础价格</th></tr></thead><tbody>{contractEntries.map(([model, setting]) => <tr key={model}><td><strong>{model}</strong></td><td>{billingLabel(setting)}</td><td>{priceSummary(setting)}</td></tr>)}</tbody></table></div>
			<div className="mirror-adoption-actions"><span>点击后创建关系并立即完成第一次同步。</span><Button disabled={!!busy || (!adoptionPreview.executable && !adoptionPreview.already_adopted)} onClick={() => void adoptAndSync()}>{busy === 'create' ? <LoaderCircle className="spin" size={16} /> : <Play size={16} />}创建并同步</Button></div>
		</Section>}

		<Section title="镜像关系" description="一条记录就是一组上游分组与本站分组的对应关系。">
			{visiblePolicies.length === 0 ? <EmptyState icon={<Workflow size={22} />} title="还没有分组镜像" description="从上方选择上游分组和本站分组即可创建。" /> : <div className="table-wrap"><table><thead><tr><th>上游分组</th><th>本站分组</th><th>执行方式</th><th>状态</th><th>最后同步</th><th>操作</th></tr></thead><tbody>{visiblePolicies.map((policy) => {
				const change = lastChange.get(policy.policy_uuid);
				const editing = editingPolicy?.policy_uuid === policy.policy_uuid;
				return <tr key={policy.policy_uuid}><td><strong>{suppliers.find((item) => item.id === policy.supplier_id)?.name ?? `上游 #${policy.supplier_id}`}</strong><small>{policy.source_group}</small></td><td><strong>{targets.find((item) => item.id === policy.target_site_id)?.name ?? `本站 #${policy.target_site_id}`}</strong><small>{policy.target_group}</small></td><td>{editing ? <div className="mirror-inline-edit"><select value={editExecutionMode} onChange={(event) => setEditExecutionMode(event.target.value as 'auto' | 'approval')}><option value="auto">自动同步</option><option value="approval">手动检查</option></select><textarea rows={3} value={editExcludedModels} onChange={(event) => setEditExcludedModels(event.target.value)} placeholder="例外模型，每行一个" /></div> : <span>{policy.execution_mode === 'approval' ? '手动检查' : '自动同步'}{(policy.excluded_models?.length ?? 0) > 0 && <small>例外 {policy.excluded_models.length} 个</small>}</span>}</td><td><StatusBadge status={policy.enabled ? 'healthy' : 'warning'} label={policy.enabled ? '运行中' : '已暂停'} /></td><td>{change ? <><strong>{change.status === 'applied' ? '同步成功' : change.status}</strong><small>{formatDateTime(change.updated_at)}</small></> : <span>尚无后台同步记录</span>}</td><td><div className="table-actions">{editing ? <><Button variant="secondary" onClick={() => void saveEdit()} disabled={!!busy}><Save size={15} />保存</Button><Button variant="ghost" onClick={() => setEditingPolicy(null)} disabled={!!busy}>取消</Button></> : <><Button variant="secondary" onClick={() => void syncPolicy(policy)} disabled={!!busy || !policy.enabled}>{busy === `sync:${policy.policy_uuid}` ? <LoaderCircle className="spin" size={15} /> : <RefreshCw size={15} />}立即同步</Button><Button variant="ghost" onClick={() => startEdit(policy)} disabled={!!busy}><Pencil size={15} />编辑</Button><Button variant="ghost" onClick={() => void togglePolicy(policy)} disabled={!!busy}>{policy.enabled ? <PauseCircle size={15} /> : <Play size={15} />}{policy.enabled ? '暂停' : '启用'}</Button><Button variant="danger" onClick={() => setDeleteCandidate(policy)} disabled={!!busy}><Trash2 size={15} />删除</Button></>}</div></td></tr>;
			})}</tbody></table></div>}
		</Section>

		{mirrorPreview && <Section title="最近一次检查" description="只展示管理员需要处理的结果。">
			<div className="mirror-preview-status"><StatusBadge status={mirrorPreview.executable ? 'healthy' : 'warning'} label={mirrorPreview.executable ? '可以同步' : '暂不能同步'} /><span>来源分组：{mirrorPreview.source_state === 'present' ? '正常' : mirrorPreview.source_state === 'empty' ? '暂无模型' : '未找到'} · 本站渠道 #{mirrorPreview.channel_id}</span></div>
			{(mirrorPreview.service_gates ?? []).filter((gate) => gate.blocks_apply).map((gate) => <div className="mirror-error" key={gate.code}><AlertTriangle size={15} /><span>{gate.message}</span></div>)}
		</Section>}

		<Section title="同步记录" description="最近的自动或手动同步结果。">
			{changes.length === 0 ? <EmptyState icon={<RefreshCw size={22} />} title="暂无同步记录" description="创建镜像后，后续同步结果会显示在这里。" /> : <div className="table-wrap"><table><thead><tr><th>关系</th><th>结果</th><th>时间</th><th>说明</th></tr></thead><tbody>{changes.slice(0, 30).map((change) => <tr key={change.id}><td>{policies.find((policy) => policy.policy_uuid === change.policy_uuid)?.target_group ?? change.policy_uuid.slice(0, 8)}</td><td>{change.status === 'applied' ? '同步成功' : change.status}</td><td>{formatDateTime(change.updated_at)}</td><td>{change.error || '模型、价格和计费方式已读回确认'}</td></tr>)}</tbody></table></div>}
		</Section>

		<Modal open={!!deleteCandidate} onClose={() => { if (!busy) setDeleteCandidate(null); }} title="删除分组镜像" description="删除后不再自动同步；真实渠道、当前模型和当前价格保持不变。" footer={<><Button variant="ghost" onClick={() => setDeleteCandidate(null)} disabled={!!busy}>取消</Button><Button variant="danger" onClick={() => void deletePolicy()} disabled={!!busy}>{busy.startsWith('delete:') ? <LoaderCircle className="spin" size={16} /> : <Trash2 size={16} />}确认删除</Button></>}>
			<p>{deleteCandidate && `${deleteCandidate.source_group} → ${deleteCandidate.target_group}`}</p>
		</Modal>
	</>;
}
