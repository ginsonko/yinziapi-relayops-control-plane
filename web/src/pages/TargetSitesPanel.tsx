import { useEffect, useMemo, useState } from 'react';
import { Activity, Database, KeyRound, Pencil, Plus, Power, RefreshCw, ServerCog, Settings2, ShieldCheck, Target, TestTube2, Trash2 } from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type {
	RawCredential,
	TargetCapabilityMatrix,
	TargetChannel,
	TargetChannelPreview,
	TargetChannelRoutingUpdate,
	TargetControlOperation,
	TargetPricingOptionPreview,
	TargetLedgerSyncConfig,
	TargetLedgerSyncStatus,
	TargetDeletionImpact,
	TargetSite,
	TargetSnapshot,
} from '../api/types';
import { Button, IconButton, Modal, StatusBadge, Toggle } from '../components/ui';
import { newIdempotencyKey } from '../idempotency';

const OPTION_LABELS: Record<string, string> = {
	GroupRatio: '分组倍率', ModelRatio: '模型倍率', ModelPrice: '按次模型价格', CompletionRatio: '输出倍率',
	CacheRatio: '缓存读取倍率', CreateCacheRatio: '缓存创建倍率', ImageRatio: '图像倍率',
	AudioRatio: '音频输入倍率', AudioCompletionRatio: '音频输出倍率',
};
const OPTION_KEYS = Object.keys(OPTION_LABELS);

type Readback = {
	targetId: number;
	snapshot: TargetSnapshot;
	channels: TargetChannel[];
	catalogGroupCount: number;
	capabilities: TargetCapabilityMatrix;
	operations: TargetControlOperation[];
	ledgerSync: TargetLedgerSyncStatus | null;
	ledgerSyncError: string;
};

type RoutingEditor = {
	targetId: number;
	channel: TargetChannel;
	group: string;
	priority: string;
	weight: string;
	autoBan: boolean;
};

type ChannelConfirmation = {
	kind: 'routing' | 'status';
	targetId: number;
	channel: TargetChannel;
	preview: TargetChannelPreview;
	routing?: TargetChannelRoutingUpdate;
	status?: number;
	idempotencyKey: string;
};

function optionValues(snapshot: TargetSnapshot, key: string) {
	if (snapshot.pricing_options?.[key]) return snapshot.pricing_options[key].values;
	return key === 'GroupRatio' ? snapshot.group_ratio ?? {} : {};
}

function operationLabel(state: string) {
	return ({
		verified_success: '读回成功', rejected_before_write: '写入前拒绝', conflict_before_write: '检测到冲突',
		ambiguous_after_write: '结果待核对', rolled_back_verified: '已验证回滚', compensation_failed: '补偿失败', in_progress: '执行中',
	} as Record<string, string>)[state] ?? state;
}

function capabilityTone(available: boolean, status: string) {
	if (available) return 'healthy' as const;
	if (status === 'forbidden' || status === 'unauthorized') return 'error' as const;
	return 'warning' as const;
}

const ledgerStreamLabels: Record<string, string> = { users: '用户与角色', logs: '调用与用量', topups: '充值现金流' };

function ledgerStreamTone(status: string, hasLastKnownGood: boolean) {
	if (status === 'healthy') return 'healthy' as const;
	if (hasLastKnownGood || status === 'pending' || status === 'unsupported') return 'warning' as const;
	return 'error' as const;
}

function ledgerOverallTone(status: TargetLedgerSyncStatus | null) {
	if (!status) return 'error' as const;
	if (!status.config.enabled) return 'unknown' as const;
	if (status.streams.every((stream) => stream.status === 'healthy')) return 'healthy' as const;
	if (status.streams.some((stream) => stream.status === 'auth_required' && !stream.has_last_known_good)) return 'error' as const;
	return 'warning' as const;
}

export function TargetSitesPanel({ onChanged }: { onChanged: (message: string) => void }) {
	const [targets, setTargets] = useState<TargetSite[]>([]);
	const [credentials, setCredentials] = useState<RawCredential[]>([]);
	const [loading, setLoading] = useState(true);
	const [pageError, setPageError] = useState('');
	const [testingId, setTestingId] = useState<number | null>(null);
	const [readback, setReadback] = useState<Readback | null>(null);

	const [createOpen, setCreateOpen] = useState(false);
	const [createSaving, setCreateSaving] = useState(false);
	const [createError, setCreateError] = useState('');
	const [authMode, setAuthMode] = useState<'token' | 'password'>('token');
	const [form, setForm] = useState({ name: '本站 NewAPI', url: '', token: '', userId: '', username: '', password: '', writeMode: 'approval' });

	const [settingsTarget, setSettingsTarget] = useState<TargetSite | null>(null);
	const [settings, setSettings] = useState({ name: '', enabled: true, writeMode: 'approval' });
	const [settingsSaving, setSettingsSaving] = useState(false);
	const [settingsError, setSettingsError] = useState('');

	const [reauthTarget, setReauthTarget] = useState<TargetSite | null>(null);
	const [reauthSaving, setReauthSaving] = useState(false);
	const [reauthError, setReauthError] = useState('');
	const [reauthMode, setReauthMode] = useState<'token' | 'password'>('token');
	const [reauth, setReauth] = useState({ token: '', userId: '', username: '', password: '' });

	const [optionKey, setOptionKey] = useState('GroupRatio');
	const [optionDraft, setOptionDraft] = useState<Record<string, string>>({});
	const [newOptionName, setNewOptionName] = useState('');
	const [newOptionValue, setNewOptionValue] = useState('');
	const [optionPreview, setOptionPreview] = useState<TargetPricingOptionPreview | null>(null);
	const [optionIdempotencyKey, setOptionIdempotencyKey] = useState('');
	const [optionBusy, setOptionBusy] = useState(false);
	const [optionError, setOptionError] = useState('');

	const [channelModels, setChannelModels] = useState<Record<number, string>>({});
	const [channelResults, setChannelResults] = useState<Record<number, string>>({});
	const [routingEditor, setRoutingEditor] = useState<RoutingEditor | null>(null);
	const [channelConfirmation, setChannelConfirmation] = useState<ChannelConfirmation | null>(null);
	const [channelBusy, setChannelBusy] = useState<number | null>(null);
	const [channelError, setChannelError] = useState('');
	const [testChannel, setTestChannel] = useState<{ targetId: number; channel: TargetChannel } | null>(null);
	const [testTimeout, setTestTimeout] = useState('30');
	const [testCostAccepted, setTestCostAccepted] = useState(false);
	const [ledgerTarget, setLedgerTarget] = useState<TargetSite | null>(null);
	const [ledgerDraft, setLedgerDraft] = useState<TargetLedgerSyncConfig | null>(null);
	const [ledgerBusy, setLedgerBusy] = useState<'save' | 'run' | null>(null);
	const [ledgerError, setLedgerError] = useState('');
	const [deleteTarget, setDeleteTarget] = useState<TargetSite | null>(null);
	const [deleteImpact, setDeleteImpact] = useState<TargetDeletionImpact | null>(null);
	const [deleteConfirmName, setDeleteConfirmName] = useState('');
	const [deleteLoading, setDeleteLoading] = useState(false);
	const [deleteError, setDeleteError] = useState('');

	const loadTargets = async () => {
		setLoading(true); setPageError('');
		try {
			const [targetResult, credentialResult] = await Promise.all([api.listTargets(), api.listCredentials()]);
			setTargets(targetResult.data); setCredentials(credentialResult.data);
		} catch (error) { setPageError(getErrorMessage(error)); } finally { setLoading(false); }
	};

	useEffect(() => { void loadTargets(); }, []);

	const inspectTarget = async (target: TargetSite) => {
		setTestingId(target.id); setPageError('');
		try {
			const ledgerPromise = api.targetLedgerSync(target.id)
				.then((result) => ({ status: result.data, error: '' }))
				.catch((reason) => ({ status: null, error: getErrorMessage(reason) }));
			const [snapshot, catalog, channels, capabilities, operations, ledger] = await Promise.all([
				api.targetSnapshot(target.id), api.targetCatalog(target.id), api.listTargetChannels(target.id),
				api.targetCapabilities(target.id), api.targetOperations(target.id, 30), ledgerPromise,
			]);
			const nextKey = snapshot.data.pricing_options?.[optionKey] ? optionKey : 'GroupRatio';
			setReadback({ targetId: target.id, snapshot: snapshot.data, channels: channels.data, catalogGroupCount: catalog.data.groups.length, capabilities: capabilities.data, operations: operations.data, ledgerSync: ledger.status, ledgerSyncError: ledger.error });
			setOptionKey(nextKey); setOptionDraft({ ...optionValues(snapshot.data, nextKey) });
			setChannelModels(Object.fromEntries(channels.data.map((channel) => [channel.id, channel.models[0] ?? ''])));
			await loadTargets();
		} catch (error) { setPageError(`${target.name} 连接检查失败：${getErrorMessage(error)}`); }
		finally { setTestingId(null); }
	};

	const refreshLedgerSync = async (targetId: number) => {
		const result = await api.targetLedgerSync(targetId);
		setReadback((current) => current?.targetId === targetId ? { ...current, ledgerSync: result.data, ledgerSyncError: '' } : current);
		return result.data;
	};

	const runLedgerSync = async (targetId: number) => {
		setLedgerBusy('run'); setLedgerError('');
		try {
			const result = await api.runTargetLedgerSync(targetId);
			setReadback((current) => current?.targetId === targetId ? { ...current, ledgerSync: result.data.status, ledgerSyncError: '' } : current);
			const imported = result.data.status.streams.reduce((total, stream) => total + stream.last_imported_events, 0);
			onChanged(`经营账本同步完成，本轮新增 ${imported} 条不可变事件。`);
		} catch (error) { setLedgerError(getErrorMessage(error)); }
		finally { setLedgerBusy(null); }
	};

	const saveLedgerSync = async () => {
		if (!ledgerTarget || !ledgerDraft) return;
		setLedgerBusy('save'); setLedgerError('');
		try {
			await api.updateTargetLedgerSync(ledgerTarget.id, ledgerDraft);
			await refreshLedgerSync(ledgerTarget.id);
			setLedgerTarget(null); setLedgerDraft(null);
			onChanged('经营账本同步配置已热更新，后台调度无需重启。');
		} catch (error) { setLedgerError(getErrorMessage(error)); }
		finally { setLedgerBusy(null); }
	};

	const createTarget = async () => {
		setCreateSaving(true); setCreateError('');
		try {
			if (!form.name.trim() || !form.url.trim()) throw new Error('请填写目标站名称和完整地址。');
			if (authMode === 'token' && (!form.token.trim() || Number(form.userId) <= 0)) throw new Error('Token 模式需要管理 Token 和 root 用户 ID。');
			if (authMode === 'password' && (!form.username.trim() || !form.password)) throw new Error('账号登录模式需要用户名和密码。');
			const result = await api.connectTarget({ name: form.name.trim(), base_url: form.url.trim(), adapter: 'newapi', write_mode: form.writeMode as 'approval' | 'auto', auth_method: authMode, token: authMode === 'token' ? form.token.trim() : undefined, user_id: authMode === 'token' ? Number(form.userId) : undefined, username: authMode === 'password' ? form.username.trim() : undefined, password: authMode === 'password' ? form.password : undefined });
			setForm((current) => ({ ...current, token: '', password: '' }));
			setCreateOpen(false); await loadTargets(); await inspectTarget(result.data.target);
			onChanged('目标站凭据已验证，九类价格 Option 与渠道能力已完成只读协商。');
		} catch (error) { setCreateError(getErrorMessage(error)); } finally { setCreateSaving(false); }
	};

	const saveSettings = async () => {
		if (!settingsTarget) return;
		setSettingsSaving(true); setSettingsError('');
		try {
			const result = await api.updateTarget(settingsTarget.id, { name: settings.name.trim(), base_url: settingsTarget.base_url, adapter: settingsTarget.adapter, credential_id: settingsTarget.credential_id, enabled: settings.enabled, write_mode: settings.writeMode });
			setSettingsTarget(null); await loadTargets(); onChanged(`${result.data.name} 的本地控制设置已更新。`);
		} catch (error) { setSettingsError(getErrorMessage(error)); } finally { setSettingsSaving(false); }
	};

	const openDeleteTarget = async (target: TargetSite) => {
		setDeleteTarget(target);
		setDeleteImpact(null);
		setDeleteConfirmName('');
		setDeleteError('');
		setDeleteLoading(true);
		try {
			const result = await api.targetDeletionImpact(target.id);
			setDeleteImpact(result.data);
		} catch (error) {
			setDeleteError(getErrorMessage(error));
		} finally {
			setDeleteLoading(false);
		}
	};

	const confirmDeleteTarget = async () => {
		if (!deleteTarget || !deleteImpact) return;
		setDeleteLoading(true);
		setDeleteError('');
		try {
			await api.deleteTarget(deleteTarget.id, { confirm_name: deleteConfirmName, impact_hash: deleteImpact.impact_hash });
			if (readback?.targetId === deleteTarget.id) setReadback(null);
			const deletedName = deleteTarget.name;
			setDeleteTarget(null);
			setDeleteImpact(null);
			setDeleteConfirmName('');
			await loadTargets();
			onChanged(`${deletedName} 已从 RelayOps 删除；远端 NewAPI 和独立凭据均未被修改。`);
		} catch (error) {
			setDeleteError(getErrorMessage(error));
			try {
				const refreshed = await api.targetDeletionImpact(deleteTarget.id);
				setDeleteImpact(refreshed.data);
			} catch {
				// Keep the original error visible when the target no longer exists.
			}
		} finally {
			setDeleteLoading(false);
		}
	};

	const reauthorize = async () => {
		if (!reauthTarget) return;
		setReauthSaving(true); setReauthError('');
		try {
			const credential = credentials.find((item) => item.id === reauthTarget.credential_id);
			if (!reauthTarget.updated_at || !credential?.updated_at) throw new Error('目标或凭据版本信息尚未加载，请关闭窗口并刷新目标列表后重试。');
			if (reauthMode === 'token' && (!reauth.token.trim() || Number(reauth.userId) <= 0)) throw new Error('Token 模式需要新 Token 和 root 用户 ID。');
			if (reauthMode === 'password' && (!reauth.username.trim() || !reauth.password)) throw new Error('账号登录模式需要用户名和密码。');
			const result = await api.reauthorizeTarget(reauthTarget.id, { auth_method: reauthMode, token: reauthMode === 'token' ? reauth.token.trim() : undefined, user_id: reauthMode === 'token' ? Number(reauth.userId) : undefined, username: reauthMode === 'password' ? reauth.username.trim() : undefined, password: reauthMode === 'password' ? reauth.password : undefined, expected_target_updated_at: reauthTarget.updated_at, expected_credential_id: reauthTarget.credential_id, expected_credential_updated_at: credential.updated_at });
			setReauth({ token: '', userId: '', username: '', password: '' }); setReauthTarget(null);
			await loadTargets(); await inspectTarget(result.data.target); onChanged('新凭据已验证、原子替换并完成持久化读回。');
		} catch (error) { setReauthError(getErrorMessage(error)); } finally { setReauthSaving(false); }
	};

	const currentOption = readback ? optionValues(readback.snapshot, optionKey) : {};
	const optionChanges = useMemo(() => {
		const names = new Set([...Object.keys(currentOption), ...Object.keys(optionDraft)]);
		return [...names].filter((name) => currentOption[name] !== optionDraft[name]);
	}, [currentOption, optionDraft]);
	const invalidOptionNames = Object.entries(optionDraft).filter(([name, value]) => !name.trim() || value.trim() === '' || !Number.isFinite(Number(value)) || Number(value) < 0).map(([name]) => name || '空键');
	const selectedOptionCapability = readback?.capabilities.pricing_options[optionKey];

	const selectOption = (key: string) => {
		setOptionKey(key); setOptionDraft(readback ? { ...optionValues(readback.snapshot, key) } : {}); setOptionPreview(null); setOptionError('');
	};

	const previewOption = async () => {
		if (!readback || optionChanges.length === 0) return;
		setOptionBusy(true); setOptionError('');
		try {
			const preview = await api.previewTargetPricingOption(readback.targetId, optionKey, optionDraft);
			setOptionPreview(preview.data); setOptionIdempotencyKey(newIdempotencyKey(`pricing-${readback.targetId}-${optionKey}`));
		} catch (error) { setOptionError(getErrorMessage(error)); } finally { setOptionBusy(false); }
	};

	const applyOption = async () => {
		if (!readback || !optionPreview) return;
		setOptionBusy(true); setOptionError('');
		try {
			const result = await api.applyTargetPricingOption(readback.targetId, optionKey, { expected_hash: optionPreview.expected_hash, preview_hash: optionPreview.preview_hash, values: optionPreview.desired, idempotency_key: optionIdempotencyKey, confirm: true });
			if (result.data.result_state !== 'verified_success') throw new Error(`操作结果：${operationLabel(result.data.result_state)}${result.data.error ? `，${result.data.error}` : ''}`);
			setOptionPreview(null); onChanged(`${OPTION_LABELS[optionKey]}已写入并完成原始 Option 与可观察价格读回。`);
			const target = targets.find((item) => item.id === readback.targetId); if (target) await inspectTarget(target);
		} catch (error) { setOptionError(getErrorMessage(error)); } finally { setOptionBusy(false); }
	};

	const previewRouting = async () => {
		if (!routingEditor) return;
		setChannelBusy(routingEditor.channel.id); setChannelError('');
		try {
			const priority = Number(routingEditor.priority); const weight = Number(routingEditor.weight);
			if (!Number.isInteger(priority) || priority < 0 || !Number.isInteger(weight) || weight < 0) throw new Error('优先级和权重必须是大于或等于 0 的整数。');
			const routing = { group: routingEditor.group.trim(), priority, weight, auto_ban: routingEditor.autoBan };
			const result = await api.previewTargetChannelRouting(routingEditor.targetId, routingEditor.channel.id, routing);
			setChannelConfirmation({ kind: 'routing', targetId: routingEditor.targetId, channel: routingEditor.channel, preview: result.data, routing, idempotencyKey: newIdempotencyKey(`routing-${routingEditor.targetId}-${routingEditor.channel.id}`) }); setRoutingEditor(null);
		} catch (error) { setChannelError(getErrorMessage(error)); } finally { setChannelBusy(null); }
	};

	const previewStatus = async (targetId: number, channel: TargetChannel) => {
		setChannelBusy(channel.id); setChannelError('');
		try {
			const status = channel.status === 1 ? 2 : 1;
			const result = await api.previewTargetChannelStatus(targetId, channel.id, status);
			setChannelConfirmation({ kind: 'status', targetId, channel, preview: result.data, status, idempotencyKey: newIdempotencyKey(`status-${targetId}-${channel.id}`) });
		} catch (error) { setChannelError(getErrorMessage(error)); } finally { setChannelBusy(null); }
	};

	const applyChannelChange = async () => {
		if (!channelConfirmation) return;
		setChannelBusy(channelConfirmation.channel.id); setChannelError('');
		try {
			const common = { expected_hash: channelConfirmation.preview.expected_hash, preview_hash: channelConfirmation.preview.preview_hash, idempotency_key: channelConfirmation.idempotencyKey, confirm: true as const };
			const result = channelConfirmation.kind === 'routing'
				? await api.applyTargetChannelRouting(channelConfirmation.targetId, channelConfirmation.channel.id, { ...common, routing: channelConfirmation.routing! })
				: await api.applyTargetChannelStatus(channelConfirmation.targetId, channelConfirmation.channel.id, { ...common, status: channelConfirmation.status! });
			if (result.data.result_state !== 'verified_success') throw new Error(`操作结果：${operationLabel(result.data.result_state)}${result.data.error ? `，${result.data.error}` : ''}`);
			const changedTarget = targets.find((item) => item.id === channelConfirmation.targetId); setChannelConfirmation(null);
			onChanged('渠道变更已按预览写入并完成字段级读回。'); if (changedTarget) await inspectTarget(changedTarget);
		} catch (error) { setChannelError(getErrorMessage(error)); } finally { setChannelBusy(null); }
	};

	const runChannelTest = async () => {
		if (!testChannel) return;
		const model = channelModels[testChannel.channel.id]; const timeout = Number(testTimeout);
		setChannelBusy(testChannel.channel.id); setChannelError('');
		try {
			if (!model || !Number.isInteger(timeout) || timeout < 1 || timeout > 60 || !testCostAccepted) throw new Error('请选择模型、1 至 60 秒超时并确认可能产生费用。');
			const result = await api.controlledTestTargetChannel(testChannel.targetId, testChannel.channel.id, { model, timeout_seconds: timeout, acknowledge_cost: true, idempotency_key: newIdempotencyKey(`test-${testChannel.targetId}-${testChannel.channel.id}`) });
			const message = result.data.result_state === 'verified_success' ? `模型 ${model} 测试通过` : `${operationLabel(result.data.result_state)}：${result.data.error ?? '请查看操作记录'}`;
			setChannelResults((current) => ({ ...current, [testChannel.channel.id]: message })); setTestChannel(null); setTestCostAccepted(false);
			const target = targets.find((item) => item.id === testChannel.targetId); if (target) await inspectTarget(target);
		} catch (error) { setChannelError(getErrorMessage(error)); } finally { setChannelBusy(null); }
	};

	return <>
		<div className="target-toolbar"><div><strong>本站 NewAPI / 目标站点</strong><span>远程管理使用 HTTPS 管理接口；RelayOps 不进入模型请求链路。</span></div><Button onClick={() => { setForm({ name: '本站 NewAPI', url: '', token: '', userId: '', username: '', password: '', writeMode: 'approval' }); setCreateError(''); setCreateOpen(true); }}><Plus size={16} />添加目标站</Button></div>
		{pageError && <div className="form-error">{pageError}</div>}
		{loading ? <div className="inline-loading"><RefreshCw size={18} className="spin" />读取目标站和凭据…</div> : targets.length === 0 ? <div className="target-empty"><ServerCog size={24} /><strong>还没有目标站</strong><span>填写任意已授权的 NewAPI HTTPS 地址和管理员凭据，验证通过后即可读取与受控管理。</span><Button onClick={() => setCreateOpen(true)}>添加本站 NewAPI</Button></div> : <div className="target-list">{targets.map((targetSite) => {
			const targetReadback = readback?.targetId === targetSite.id ? readback : null;
			return <article className="target-row" key={targetSite.id}>
				<div className={`target-icon status-${targetSite.last_status === 'healthy' ? 'healthy' : targetSite.last_status === 'degraded' ? 'error' : 'unknown'}`}><Target size={19} /></div>
				<div className="target-main"><div><strong>{targetSite.name}</strong><StatusBadge status={targetSite.last_status === 'healthy' ? 'healthy' : targetSite.last_status === 'degraded' ? 'error' : 'unknown'} label={targetSite.last_status || '尚未检查'} /></div><span>{targetSite.base_url}</span><small>NewAPI · 凭据 #{targetSite.credential_id} · {targetSite.enabled ? '已启用' : '本地停用'} · {targetSite.write_mode === 'approval' ? '确认后写入' : '自动规则可写入'}</small>{targetSite.last_error && <p>{targetSite.last_error}</p>}</div>
				<div className="target-actions"><Button variant="secondary" onClick={() => void inspectTarget(targetSite)} disabled={testingId === targetSite.id}>{testingId === targetSite.id ? <RefreshCw size={15} className="spin" /> : <TestTube2 size={15} />}{testingId === targetSite.id ? '检查中' : '读取控制面'}</Button><IconButton label={`重新授权 ${targetSite.name}`} onClick={() => { setReauthTarget(targetSite); setReauth({ token: '', userId: '', username: '', password: '' }); setReauthError(''); }}><KeyRound size={16} /></IconButton><IconButton label={`设置 ${targetSite.name}`} onClick={() => { setSettingsTarget(targetSite); setSettings({ name: targetSite.name, enabled: targetSite.enabled, writeMode: targetSite.write_mode || 'approval' }); setSettingsError(''); }}><Pencil size={16} /></IconButton><IconButton className="icon-button-danger" label={`删除 ${targetSite.name}`} onClick={() => void openDeleteTarget(targetSite)}><Trash2 size={16} /></IconButton></div>
				{targetReadback && <div className="target-readback">
					<div className="target-readback-summary"><div><strong>实时管理读回成功</strong><span>{targetReadback.snapshot.contract_known ? `已识别 ${targetReadback.snapshot.contract || 'NewAPI'} 契约` : '未知契约，已自动降级为只读'}</span></div><small>快照 {targetReadback.snapshot.hash.slice(0, 12)} · {targetReadback.catalogGroupCount} 个目录分组 · {targetReadback.channels.length} 条渠道</small></div>
					<div className="target-capability-strip"><StatusBadge status={targetReadback.capabilities.read_only ? 'warning' : 'healthy'} label={targetReadback.capabilities.read_only ? '只读模式' : '受控写入可用'} /><span>Option: {targetReadback.capabilities.option_contract || '待识别'}</span><span>渠道: {targetReadback.capabilities.channel_contract || '待识别'}</span>{targetReadback.capabilities.read_only_reason && <small>{targetReadback.capabilities.read_only_reason}</small>}</div>
					<section className="target-ledger-sync-panel">
						<header><div className="target-ledger-sync-title"><Database size={17} /><div><strong>经营账本自动同步</strong><span>每五分钟增量读取用户、调用与充值；失败保留上次可信游标，不进入 NewAPI 请求链路。</span></div></div><div className="target-ledger-sync-actions">{targetReadback.ledgerSync && <StatusBadge status={ledgerOverallTone(targetReadback.ledgerSync)} label={!targetReadback.ledgerSync.config.enabled ? '已暂停' : targetReadback.ledgerSync.streams.every((stream) => stream.status === 'healthy') ? '三路正常' : '部分待处理'} />}<Button variant="secondary" onClick={() => { if (!targetReadback.ledgerSync) return; setLedgerTarget(targetSite); setLedgerDraft({ ...targetReadback.ledgerSync.config }); setLedgerError(''); }} disabled={!targetReadback.ledgerSync || ledgerBusy !== null}><Settings2 size={14} />同步设置</Button><Button onClick={() => void runLedgerSync(targetSite.id)} disabled={!targetReadback.ledgerSync?.config.enabled || ledgerBusy !== null}>{ledgerBusy === 'run' ? <RefreshCw size={14} className="spin" /> : <RefreshCw size={14} />}立即同步</Button></div></header>
						{targetReadback.ledgerSyncError ? <div className="form-error">同步状态暂不可读：{targetReadback.ledgerSyncError}</div> : targetReadback.ledgerSync && <>
							<div className="target-ledger-sync-meta"><span>周期 {Math.round(targetReadback.ledgerSync.config.interval_seconds / 60)} 分钟</span><span>首次回看 {Math.round(targetReadback.ledgerSync.config.initial_lookback_seconds / 3600)} 小时</span><span>{targetReadback.ledgerSync.effective_currency ? `${targetReadback.ledgerSync.unit_mode === 'auto' ? '自动识别' : '手动'} ${targetReadback.ledgerSync.effective_currency} · quota×${targetReadback.ledgerSync.effective_quota_cash_factor || '不折现'}` : '现金单位尚无权威证据'}</span><span>{targetReadback.ledgerSync.next_run_at ? `下次 ${new Date(targetReadback.ledgerSync.next_run_at).toLocaleString('zh-CN')}` : '等待首次调度'}</span>{targetReadback.ledgerSync.unit_warning && <small>{targetReadback.ledgerSync.unit_warning}</small>}</div>
							<div className="target-ledger-streams">{targetReadback.ledgerSync.streams.map((stream) => <article key={stream.stream}><StatusBadge status={ledgerStreamTone(stream.status, stream.has_last_known_good)} label={stream.status === 'healthy' ? '正常' : stream.status === 'auth_required' ? '授权失效' : stream.status === 'unsupported' ? '端点不支持' : stream.status === 'failed' ? '本轮失败' : '等待首次同步'} /><div><strong>{ledgerStreamLabels[stream.stream] ?? stream.stream}</strong><span>{stream.next_action}</span></div><div><strong>新增 {stream.last_imported_events}</strong><span>扫描 {stream.last_scanned_rows} · 跳过 {stream.last_skipped_rows}</span></div><small>{stream.last_success_at ? `最近成功 ${new Date(stream.last_success_at).toLocaleString('zh-CN')}` : '尚无可信快照'}{stream.last_error ? ` · ${stream.last_error}` : ''}</small></article>)}</div>
						</>}
						{ledgerError && <div className="form-error">{ledgerError}</div>}
					</section>
					<div className="target-control-grid">
						<section className="target-control-panel"><header><div><strong>价格 Option</strong><span>九个维度独立哈希、预览、确认、写入和读回。</span></div><b>{optionChanges.length > 0 ? `${optionChanges.length} 项待预览` : '与目标站一致'}</b></header>
							<div className="target-option-picker"><select aria-label="价格 Option" value={optionKey} onChange={(event) => selectOption(event.target.value)}>{OPTION_KEYS.map((key) => { const capability = targetReadback.capabilities.pricing_options[key]; return <option key={key} value={key}>{OPTION_LABELS[key]} · {capability?.read.available ? `${capability.count} 项` : '不可读'}</option>; })}</select>{selectedOptionCapability && <StatusBadge status={capabilityTone(selectedOptionCapability.write.available, selectedOptionCapability.write.status)} label={selectedOptionCapability.write.available ? '可写' : selectedOptionCapability.write.reason || selectedOptionCapability.write.status} />}</div>
							<div className="ratio-editor">{Object.entries(optionDraft).sort(([a], [b]) => a.localeCompare(b, 'zh-CN')).map(([name, value]) => <label key={name} className={currentOption[name] !== value ? 'is-changed' : ''}><span><b>{name}</b><small>{currentOption[name] === undefined ? '新增项' : `当前 ${currentOption[name]}`}</small></span><input inputMode="decimal" aria-label={`${name} 的 ${OPTION_LABELS[optionKey]}`} value={value} onChange={(event) => { setOptionDraft((current) => ({ ...current, [name]: event.target.value })); setOptionPreview(null); }} /><IconButton label={`删除 ${name}`} onClick={() => { const next = { ...optionDraft }; delete next[name]; setOptionDraft(next); setOptionPreview(null); }}><Trash2 size={14} /></IconButton></label>)}</div>
							<div className="target-option-add"><input value={newOptionName} onChange={(event) => setNewOptionName(event.target.value)} placeholder="新增键名" /><input inputMode="decimal" value={newOptionValue} onChange={(event) => setNewOptionValue(event.target.value)} placeholder="数值" /><Button variant="secondary" onClick={() => { const name = newOptionName.trim(); if (!name || optionDraft[name] !== undefined) return; setOptionDraft({ ...optionDraft, [name]: newOptionValue.trim() }); setNewOptionName(''); setNewOptionValue(''); setOptionPreview(null); }} disabled={!newOptionName.trim() || !newOptionValue.trim() || optionDraft[newOptionName.trim()] !== undefined}><Plus size={14} />添加</Button></div>
							{invalidOptionNames.length > 0 && <div className="form-error">键名不能为空，数值必须大于或等于 0：{invalidOptionNames.join('、')}</div>}{optionError && <div className="form-error">{optionError}</div>}
							<div className="target-panel-actions"><Button variant="secondary" onClick={() => setOptionDraft({ ...currentOption })} disabled={optionChanges.length === 0 || optionBusy}>恢复读回值</Button><Button onClick={() => void previewOption()} disabled={optionChanges.length === 0 || invalidOptionNames.length > 0 || optionBusy || !selectedOptionCapability?.write.available}>{optionBusy && <RefreshCw size={14} className="spin" />}查看差异并确认</Button></div>
						</section>
						<section className="target-control-panel"><header><div><strong>渠道管理</strong><span>路由、启停和可能计费的真实测试分开授权。</span></div><b>{targetReadback.channels.filter((channel) => channel.status === 1).length} 条启用</b></header>
							<div className="target-channel-list">{targetReadback.channels.length === 0 ? <div className="target-channel-empty">目标站当前没有渠道。</div> : targetReadback.channels.map((channel) => <div className="target-channel-row" key={channel.id}><div className="target-channel-name"><span><strong>{channel.name || `渠道 #${channel.id}`}</strong><StatusBadge status={channel.status === 1 ? 'healthy' : 'warning'} label={channel.status === 1 ? '已启用' : `状态 ${channel.status}`} /></span><small>#{channel.id} · {channel.group || '未分组'} · 优先级 {channel.priority} · 权重 {channel.weight}</small></div><div className="target-channel-actions"><IconButton label={`配置 ${channel.name || channel.id} 路由`} onClick={() => { setRoutingEditor({ targetId: targetSite.id, channel, group: channel.group, priority: String(channel.priority), weight: String(channel.weight), autoBan: channel.auto_ban }); setChannelError(''); }} disabled={!targetReadback.capabilities.channels.routing_write.available}><Settings2 size={15} /></IconButton><IconButton label={channel.status === 1 ? `停用 ${channel.name}` : `启用 ${channel.name}`} onClick={() => void previewStatus(targetSite.id, channel)} disabled={channelBusy !== null || !targetReadback.capabilities.channels.status_write.available}><Power size={15} /></IconButton></div><div className="target-channel-test"><select aria-label={`${channel.name} 测试模型`} value={channelModels[channel.id] ?? ''} onChange={(event) => setChannelModels((current) => ({ ...current, [channel.id]: event.target.value }))} disabled={channel.models.length === 0}>{channel.models.length === 0 ? <option value="">无可测模型</option> : channel.models.map((model) => <option key={model} value={model}>{model}</option>)}</select><Button variant="secondary" onClick={() => { setTestChannel({ targetId: targetSite.id, channel }); setTestCostAccepted(false); setChannelError(''); }} disabled={channelBusy !== null || !channelModels[channel.id] || !targetReadback.capabilities.channels.billable_test.available}><TestTube2 size={14} />真实测试</Button></div>{channelResults[channel.id] && <p className={channelResults[channel.id].includes('通过') ? 'is-success' : 'is-error'}>{channelResults[channel.id]}</p>}</div>)}</div>
						</section>
					</div>
					<section className="target-operation-panel"><header><Activity size={16} /><div><strong>最近受控操作</strong><span>每次写入、冲突、回滚和计费测试都有持久化回执。</span></div></header>{targetReadback.operations.length === 0 ? <p>尚无写入操作。</p> : <div>{targetReadback.operations.slice(0, 12).map((operation) => <article key={operation.id}><StatusBadge status={operation.result_state === 'verified_success' ? 'healthy' : operation.result_state === 'in_progress' ? 'warning' : 'error'} label={operationLabel(operation.result_state)} /><strong>{operation.action}</strong><span>{new Date(operation.started_at).toLocaleString('zh-CN')}</span>{operation.error && <small>{operation.error}</small>}</article>)}</div>}</section>
				</div>}
			</article>;
		})}</div>}

		<Modal open={createOpen} onClose={() => !createSaving && setCreateOpen(false)} title="添加本站 NewAPI" description="先用只读管理接口验证权限，验证通过后才会加密保存凭据和创建目标。" footer={<><Button variant="ghost" onClick={() => setCreateOpen(false)} disabled={createSaving}>取消</Button><Button onClick={() => void createTarget()} disabled={createSaving}>{createSaving && <RefreshCw size={16} className="spin" />}{createSaving ? '验证并连接中' : '验证权限并连接'}</Button></>}>
			<div className="form-grid"><label className="field full"><span>目标站名称</span><input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /></label><label className="field full"><span>NewAPI HTTPS 地址</span><input value={form.url} onChange={(event) => setForm({ ...form, url: event.target.value })} placeholder="https://api.example.com" /></label><label className="field"><span>连接器</span><select value="newapi" disabled><option>NewAPI</option></select></label><label className="field"><span>写入模式</span><select value={form.writeMode} onChange={(event) => setForm({ ...form, writeMode: event.target.value })}><option value="approval">每次确认后写入</option><option value="auto">允许已批准规则自动写入</option></select></label></div>
			<div className="segmented credential-type-tabs"><button className={authMode === 'token' ? 'active' : ''} onClick={() => { setAuthMode('token'); setCreateError(''); }}>管理 API Token</button><button className={authMode === 'password' ? 'active' : ''} onClick={() => { setAuthMode('password'); setCreateError(''); }}>管理员账号登录</button></div>{authMode === 'token' ? <div className="form-grid"><label className="field"><span>root 用户 ID</span><input type="number" min="1" value={form.userId} onChange={(event) => setForm({ ...form, userId: event.target.value })} /></label><label className="field"><span>管理 API Token</span><input type="password" autoComplete="off" value={form.token} onChange={(event) => setForm({ ...form, token: event.target.value })} /></label></div> : <div className="form-grid"><label className="field"><span>管理员用户名</span><input autoComplete="username" value={form.username} onChange={(event) => setForm({ ...form, username: event.target.value })} /></label><label className="field"><span>管理员密码</span><input type="password" autoComplete="current-password" value={form.password} onChange={(event) => setForm({ ...form, password: event.target.value })} /></label></div>}
			<div className="info-callout"><ShieldCheck size={18} /><div><strong>权限按能力逐项协商</strong><span>密码只用于本次换取会话；未知版本或权限不足会降级为只读，不会试探性写入。</span></div></div>{createError && <div className="form-error">{createError}</div>}
		</Modal>

		<Modal open={!!settingsTarget} onClose={() => !settingsSaving && setSettingsTarget(null)} title="目标站本地设置" description="目标地址和凭据来源不会在这里静默替换；更换授权请使用钥匙按钮。" footer={<><Button variant="ghost" onClick={() => setSettingsTarget(null)} disabled={settingsSaving}>取消</Button><Button onClick={() => void saveSettings()} disabled={settingsSaving || !settings.name.trim()}>{settingsSaving ? '保存中' : '保存设置'}</Button></>}>
			{settingsTarget && <div className="form-grid"><label className="field full"><span>目标站名称</span><input value={settings.name} onChange={(event) => setSettings({ ...settings, name: event.target.value })} /></label><label className="field full"><span>已绑定地址</span><input value={settingsTarget.base_url} disabled /></label><label className="field"><span>规则写入模式</span><select value={settings.writeMode} onChange={(event) => setSettings({ ...settings, writeMode: event.target.value })}><option value="approval">每次确认后写入</option><option value="auto">允许已批准规则自动写入</option></select></label><label className="toggle-label"><Toggle checked={settings.enabled} onChange={(enabled) => setSettings({ ...settings, enabled })} label="启用目标站" />启用目标站</label></div>}{settingsError && <div className="form-error">{settingsError}</div>}
		</Modal>

		<Modal open={!!deleteTarget} onClose={() => !deleteLoading && setDeleteTarget(null)} title={`删除 ${deleteTarget?.name ?? '目标站'}`} description="只删除 RelayOps 内的目标配置和专属运行数据，不会向远端 NewAPI 发送删除请求，也不会删除独立凭据。" footer={<><Button variant="ghost" onClick={() => setDeleteTarget(null)} disabled={deleteLoading}>取消</Button><Button variant="danger" onClick={() => void confirmDeleteTarget()} disabled={deleteLoading || !deleteImpact || deleteConfirmName.trim() !== deleteImpact.target_name || deleteImpact.active_leases > 0}>{deleteLoading && <RefreshCw size={16} className="spin" />}{deleteLoading ? '正在核对' : '确认删除'}</Button></>}>
			{deleteLoading && !deleteImpact ? <div className="inline-loading"><RefreshCw size={18} className="spin" />核对关联规则、账本和受控操作…</div> : deleteImpact && <div className="deletion-impact">
				<div className="danger-callout"><Trash2 size={18} /><div><strong>这是 RelayOps 本地删除</strong><span>远端站点、用户数据库、渠道、额度、价格和凭据密文均不会被修改。</span></div></div>
				<div className="deletion-impact-grid">
					<div><span>定价配置</span><strong>{deleteImpact.pricing_policies}</strong><small>规则及其未执行建议</small></div>
					<div><span>变更与决策证据</span><strong>{deleteImpact.change_proposals + deleteImpact.decision_records}</strong><small>提案、预测和学习记录</small></div>
					<div><span>上架与控制记录</span><strong>{deleteImpact.listing_plans + deleteImpact.listing_change_sets + deleteImpact.control_operations}</strong><small>目标专属运行数据</small></div>
					<div><span>经营与机会数据</span><strong>{deleteImpact.ledger_sync_records + deleteImpact.ledger_events + deleteImpact.opportunity_records}</strong><small>同步状态、账本与用量</small></div>
				</div>
				<div className="credential-preserved-note"><ShieldCheck size={17} /><div><strong>凭据 #{deleteImpact.credential_id} 将保留</strong><span>当前另有 {Math.max(0, deleteImpact.credential_supplier_refs + deleteImpact.credential_target_refs - 1)} 个站点引用；即使没有其它引用，也不会在本次操作中删除。</span></div></div>
				{deleteImpact.active_leases > 0 && <div className="form-error">当前仍有 {deleteImpact.active_leases} 个受控操作正在执行，完成后请重新打开删除窗口。</div>}
				<label className="field"><span>输入完整名称以确认</span><input autoComplete="off" value={deleteConfirmName} onChange={(event) => setDeleteConfirmName(event.target.value)} placeholder={deleteImpact.target_name} /><small>必须精确输入：{deleteImpact.target_name}</small></label>
			</div>}
			{deleteError && <div className="form-error">{deleteError}</div>}
		</Modal>

		<Modal open={!!reauthTarget} onClose={() => !reauthSaving && setReauthTarget(null)} title={`重新授权 ${reauthTarget?.name ?? ''}`} description="新凭据先在内存中验证，原子替换并再次读回；任一步失败都会保留旧凭据。" footer={<><Button variant="ghost" onClick={() => setReauthTarget(null)} disabled={reauthSaving}>取消</Button><Button onClick={() => void reauthorize()} disabled={reauthSaving}>{reauthSaving && <RefreshCw size={16} className="spin" />}{reauthSaving ? '验证并替换中' : '验证并原子替换'}</Button></>}>
			<div className="segmented credential-type-tabs"><button className={reauthMode === 'token' ? 'active' : ''} onClick={() => { setReauthMode('token'); setReauthError(''); }}>新管理 Token</button><button className={reauthMode === 'password' ? 'active' : ''} onClick={() => { setReauthMode('password'); setReauthError(''); }}>管理员账号登录</button></div>{reauthMode === 'token' ? <div className="form-grid"><label className="field"><span>root 用户 ID</span><input type="number" min="1" value={reauth.userId} onChange={(event) => setReauth({ ...reauth, userId: event.target.value })} /></label><label className="field"><span>新管理 Token</span><input type="password" autoComplete="off" value={reauth.token} onChange={(event) => setReauth({ ...reauth, token: event.target.value })} /></label></div> : <div className="form-grid"><label className="field"><span>管理员用户名</span><input autoComplete="username" value={reauth.username} onChange={(event) => setReauth({ ...reauth, username: event.target.value })} /></label><label className="field"><span>管理员密码</span><input type="password" autoComplete="current-password" value={reauth.password} onChange={(event) => setReauth({ ...reauth, password: event.target.value })} /></label></div>}{reauthError && <div className="form-error">{reauthError}</div>}
		</Modal>

		<Modal open={!!ledgerTarget && !!ledgerDraft} onClose={() => ledgerBusy === null && setLedgerTarget(null)} title={`经营账本同步 · ${ledgerTarget?.name ?? ''}`} description="配置保存后由后台热更新执行。关闭只暂停新同步，不删除已经进入不可变账本的证据。" footer={<><Button variant="ghost" onClick={() => setLedgerTarget(null)} disabled={ledgerBusy !== null}>取消</Button><Button onClick={() => void saveLedgerSync()} disabled={ledgerBusy !== null}>{ledgerBusy === 'save' && <RefreshCw size={16} className="spin" />}{ledgerBusy === 'save' ? '保存中' : '保存设置'}</Button></>}>
			{ledgerDraft && <div className="form-grid ledger-sync-form"><label className="toggle-label full"><Toggle checked={ledgerDraft.enabled} onChange={(enabled) => setLedgerDraft({ ...ledgerDraft, enabled })} label="启用自动同步" />启用自动增量同步</label><label className="field"><span>同步周期（秒）</span><input type="number" min="60" max="86400" value={ledgerDraft.interval_seconds} onChange={(event) => setLedgerDraft({ ...ledgerDraft, interval_seconds: Number(event.target.value) })} /><small>默认 300 秒；后台每 30 秒检查到期目标</small></label><label className="field"><span>重叠去重窗口（秒）</span><input type="number" min="0" max="86400" value={ledgerDraft.overlap_seconds} onChange={(event) => setLedgerDraft({ ...ledgerDraft, overlap_seconds: Number(event.target.value) })} /><small>重复行使用稳定事件 ID 幂等去重</small></label><label className="field"><span>首次回看（秒）</span><input type="number" min="300" max="7776000" value={ledgerDraft.initial_lookback_seconds} onChange={(event) => setLedgerDraft({ ...ledgerDraft, initial_lookback_seconds: Number(event.target.value) })} /></label><label className="field"><span>每页 / 最大页数</span><div className="ledger-sync-paging"><input type="number" min="10" max="500" value={ledgerDraft.page_size} onChange={(event) => setLedgerDraft({ ...ledgerDraft, page_size: Number(event.target.value) })} /><input type="number" min="1" max="500" value={ledgerDraft.max_pages} onChange={(event) => setLedgerDraft({ ...ledgerDraft, max_pages: Number(event.target.value) })} /></div></label><label className="field"><span>现金币种</span><input maxLength={3} value={ledgerDraft.currency ?? ''} placeholder="留空自动识别" onChange={(event) => setLedgerDraft({ ...ledgerDraft, currency: event.target.value.toUpperCase() })} /><small>币种与因子都留空时，从目标 `/api/status` 自动取证</small></label><label className="field"><span>quota 到现金因子</span><input inputMode="decimal" value={ledgerDraft.quota_cash_factor ?? ''} placeholder="留空自动识别" onChange={(event) => setLedgerDraft({ ...ledgerDraft, quota_cash_factor: event.target.value })} /><small>手动填写时覆盖自动值；只填币种则仅归类充值，不折算调用</small></label><details className="ledger-sync-advanced full"><summary>高级接口路径</summary><div className="form-grid"><label className="field"><span>调用日志</span><input value={ledgerDraft.log_path} onChange={(event) => setLedgerDraft({ ...ledgerDraft, log_path: event.target.value })} /></label><label className="field"><span>充值记录</span><input value={ledgerDraft.topup_path} onChange={(event) => setLedgerDraft({ ...ledgerDraft, topup_path: event.target.value })} /></label><label className="field full"><span>用户与角色</span><input value={ledgerDraft.user_path} onChange={(event) => setLedgerDraft({ ...ledgerDraft, user_path: event.target.value })} /></label></div></details></div>}{ledgerError && <div className="form-error">{ledgerError}</div>}
		</Modal>

		<Modal open={!!optionPreview} onClose={() => !optionBusy && setOptionPreview(null)} title={`确认更新${OPTION_LABELS[optionKey]}`} description="服务端会写前复读、串行租约、写后读回，并只在仍符合条件时补偿。" footer={<><Button variant="ghost" onClick={() => setOptionPreview(null)} disabled={optionBusy}>取消</Button><Button onClick={() => void applyOption()} disabled={optionBusy || optionPreview?.no_change}>{optionBusy && <RefreshCw size={16} className="spin" />}{optionBusy ? '写入并读回中' : '确认执行'}</Button></>}>
			{optionPreview && <div className="ratio-confirm"><div><span>目标 Option</span><strong>{optionPreview.option}</strong></div><div><span>预读哈希</span><code>{optionPreview.expected_hash}</code></div><div className="ratio-change-list">{optionPreview.diff.map((diff) => <p key={`${diff.kind}-${diff.field}`}><b>{diff.field}</b><span>{diff.before || '未设置'}</span><strong>→</strong><span>{diff.after || '删除'}</span></p>)}</div>{optionPreview.deletions.length > 0 && <div className="warning-callout">将删除：{optionPreview.deletions.join('、')}</div>}<small>此确认仅对当前预览哈希和幂等键有效；目标被外部修改时会停止。</small>{optionError && <div className="form-error">{optionError}</div>}</div>}
		</Modal>

		<Modal open={!!routingEditor} onClose={() => channelBusy === null && setRoutingEditor(null)} title={`配置渠道 ${routingEditor?.channel.name ?? ''}`} description="这里只修改分组、优先级、权重和自动禁用，不会触碰密钥、模型或地址。" footer={<><Button variant="ghost" onClick={() => setRoutingEditor(null)} disabled={channelBusy !== null}>取消</Button><Button onClick={() => void previewRouting()} disabled={channelBusy !== null}>{channelBusy !== null && <RefreshCw size={16} className="spin" />}生成差异预览</Button></>}>
			{routingEditor && <div className="form-grid"><label className="field full"><span>渠道分组</span><input value={routingEditor.group} onChange={(event) => setRoutingEditor({ ...routingEditor, group: event.target.value })} /></label><label className="field"><span>优先级</span><input type="number" min="0" step="1" value={routingEditor.priority} onChange={(event) => setRoutingEditor({ ...routingEditor, priority: event.target.value })} /></label><label className="field"><span>权重</span><input type="number" min="0" step="1" value={routingEditor.weight} onChange={(event) => setRoutingEditor({ ...routingEditor, weight: event.target.value })} /></label><label className="toggle-label full"><Toggle checked={routingEditor.autoBan} onChange={(autoBan) => setRoutingEditor({ ...routingEditor, autoBan })} label="允许 NewAPI 自动禁用" />允许 NewAPI 自动禁用异常渠道</label></div>}{channelError && <div className="form-error">{channelError}</div>}
		</Modal>

		<Modal open={!!channelConfirmation} onClose={() => channelBusy === null && setChannelConfirmation(null)} title={channelConfirmation?.kind === 'routing' ? '确认渠道路由变更' : channelConfirmation?.status === 1 ? '确认启用渠道' : '确认停用渠道'} description="差异只对当前渠道配置哈希有效，写入后会立即读回。" footer={<><Button variant="ghost" onClick={() => setChannelConfirmation(null)} disabled={channelBusy !== null}>取消</Button><Button onClick={() => void applyChannelChange()} disabled={channelBusy !== null || channelConfirmation?.preview.no_change}>{channelBusy !== null && <RefreshCw size={16} className="spin" />}确认执行</Button></>}>
			{channelConfirmation && <div className="ratio-confirm"><div><span>渠道</span><strong>{channelConfirmation.channel.name || `#${channelConfirmation.channel.id}`}</strong></div><div className="ratio-change-list">{channelConfirmation.preview.diff.map((diff) => <p key={diff.field}><b>{diff.field}</b><span>{diff.before || '未设置'}</span><strong>→</strong><span>{diff.after || '未设置'}</span></p>)}</div>{channelError && <div className="form-error">{channelError}</div>}</div>}
		</Modal>

		<Modal open={!!testChannel} onClose={() => channelBusy === null && setTestChannel(null)} title="渠道真实测试" description="该操作会让目标 NewAPI 调用选定渠道，可能产生少量上游费用，并受服务端冷却限制。" footer={<><Button variant="ghost" onClick={() => setTestChannel(null)} disabled={channelBusy !== null}>取消</Button><Button onClick={() => void runChannelTest()} disabled={channelBusy !== null || !testCostAccepted}>{channelBusy !== null && <RefreshCw size={16} className="spin" />}确认并测试</Button></>}>
			{testChannel && <div className="form-grid"><label className="field full"><span>测试模型</span><select value={channelModels[testChannel.channel.id] ?? ''} onChange={(event) => setChannelModels((current) => ({ ...current, [testChannel.channel.id]: event.target.value }))}>{testChannel.channel.models.map((model) => <option key={model}>{model}</option>)}</select></label><label className="field"><span>超时时间（秒）</span><input type="number" min="1" max="60" value={testTimeout} onChange={(event) => setTestTimeout(event.target.value)} /></label><label className="check-row full"><input type="checkbox" checked={testCostAccepted} onChange={(event) => setTestCostAccepted(event.target.checked)} /><span>我确认这次真实测试可能产生上游费用</span></label></div>}{channelError && <div className="form-error">{channelError}</div>}
		</Modal>
	</>;
}
