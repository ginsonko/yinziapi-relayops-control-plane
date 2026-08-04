import { useMemo, useState, type ChangeEvent } from 'react';
import {
	AlertTriangle,
	ArchiveRestore,
	Check,
	CircleHelp,
	Download,
	FileJson,
	History,
	LoaderCircle,
	RefreshCw,
	ShieldCheck,
	Upload,
} from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type {
	ConfigImportMode,
	ConfigImportPreview,
	ConfigImportRun,
	ConfigImportSummary,
	ConfigObjectAction,
	PortableConfig,
} from '../api/types';
import { Button, Modal, PageHeader, Section, StatusBadge } from '../components/ui';

type ActionFilter = 'all' | 'conflict' | 'pending';
type BusyAction = 'export' | 'preview' | 'apply' | 'read' | 'rollback' | '';

const modes: Array<{ value: ConfigImportMode; label: string; description: string }> = [
	{ value: 'add', label: '安全新增', description: '新增缺少的对象，遇到同一站点或规则时保留本地配置。' },
	{ value: 'overwrite', label: '覆盖同步', description: '按预览覆盖冲突对象，适合让当前实例跟随导出配置。' },
	{ value: 'copy', label: '复制规则', description: '复用唯一站点，并为可复制的规则生成停用副本供复核。' },
];

const objectLabels: Record<string, string> = {
	credential: '凭据引用',
	connector_spec: '连接器',
	supplier: '上游站点',
	target: '目标站点',
	pricing_policy: '价格规则',
	setting: '系统设置',
};

const actionLabels: Record<string, string> = {
	create: '新增',
	overwrite: '覆盖',
	reuse: '保留本地',
	copy: '创建副本',
	resolve: '已匹配',
	unresolved: '待补配置',
	pending_disabled: '待补后启用',
	skipped: '跳过',
};

const pendingActions = new Set(['unresolved', 'pending_disabled', 'skipped']);

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function portableBundleFromJSON(value: unknown): PortableConfig {
	let candidate = value;
	if (isRecord(candidate) && isRecord(candidate.data)) candidate = candidate.data;
	if (isRecord(candidate) && isRecord(candidate.bundle)) candidate = candidate.bundle;
	if (!isRecord(candidate) || typeof candidate.schema_version !== 'string') {
		throw new Error('文件中没有找到可导入的 RelayOps 配置 bundle。');
	}
	return candidate as unknown as PortableConfig;
}

function bundleCount(bundle: PortableConfig) {
	return [bundle.suppliers, bundle.targets, bundle.pricing_policies, bundle.connector_specs, bundle.settings]
		.reduce((total, items) => total + (Array.isArray(items) ? items.length : 0), 0);
}

function formatTime(value?: string) {
	if (!value) return '—';
	const parsed = new Date(value);
	return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString('zh-CN', { hour12: false });
}

function shortHash(value?: string) {
	if (!value) return '—';
	return value.length > 20 ? `${value.slice(0, 10)}…${value.slice(-8)}` : value;
}

function runStatus(run: ConfigImportRun) {
	if (run.status === 'applied') return { health: 'healthy' as const, label: '已应用' };
	if (run.status === 'rolled_back') return { health: 'warning' as const, label: '已回滚' };
	return { health: 'error' as const, label: run.status || '状态未知' };
}

function SummaryStrip({ summary }: { summary: ConfigImportSummary }) {
	const items = [
		['新增', summary.create],
		['覆盖', summary.overwrite],
		['复用', summary.reuse],
		['副本', summary.copy],
		['停用', summary.disabled],
		['待补', summary.unresolved],
		['跳过', summary.skipped],
	] as const;
	return <div className="config-summary-strip">{items.map(([label, value]) => <div key={label} className={label === '待补' && value > 0 ? 'has-warning' : ''}><span>{label}</span><strong>{value}</strong></div>)}</div>;
}

function ActionRows({ actions }: { actions: ConfigObjectAction[] }) {
	if (actions.length === 0) return <div className="config-action-empty"><Check size={18} />当前筛选条件下没有对象。</div>;
	return <div className="config-action-list">{actions.map((action, index) => {
		const pending = pendingActions.has(action.action);
		return <div className={`config-action-row ${pending ? 'is-pending' : ''}`} key={`${action.object_type}-${action.object_key}-${index}`}>
			<div className="config-action-kind"><span>{objectLabels[action.object_type] ?? action.object_type}</span><strong title={action.object_key}>{action.object_key}</strong></div>
			<div className="config-action-state">
				<span className={`config-action-pill action-${action.action}`}>{actionLabels[action.action] ?? action.action}</span>
				{action.conflict && <span className="config-conflict-pill">存在冲突</span>}
				{!action.enabled && <span className="config-disabled-pill">保持停用</span>}
			</div>
			{(action.resolved_object || (action.reasons?.length ?? 0) > 0) && <div className="config-action-reasons">
				{action.resolved_object && <small>匹配到：{action.resolved_object}</small>}
				{action.reasons?.map((reason) => <small key={reason}>{reason}</small>)}
			</div>}
		</div>;
	})}</div>;
}

export function ConfigPortabilityPage() {
	const [mode, setMode] = useState<ConfigImportMode>('add');
	const [bundle, setBundle] = useState<PortableConfig | null>(null);
	const [fileName, setFileName] = useState('');
	const [preview, setPreview] = useState<ConfigImportPreview | null>(null);
	const [run, setRun] = useState<ConfigImportRun | null>(null);
	const [runId, setRunId] = useState('');
	const [filter, setFilter] = useState<ActionFilter>('all');
	const [busy, setBusy] = useState<BusyAction>('');
	const [error, setError] = useState('');
	const [notice, setNotice] = useState('');
	const [applyOpen, setApplyOpen] = useState(false);
	const [applyConfirmed, setApplyConfirmed] = useState(false);
	const [rollbackOpen, setRollbackOpen] = useState(false);
	const [rollbackConfirmed, setRollbackConfirmed] = useState(false);

	const filteredActions = useMemo(() => {
		if (!preview) return [];
		if (filter === 'conflict') return preview.actions.filter((item) => item.conflict);
		if (filter === 'pending') return preview.actions.filter((item) => pendingActions.has(item.action));
		return preview.actions;
	}, [filter, preview]);

	const conflictCount = preview?.actions.filter((item) => item.conflict).length ?? 0;
	const pendingCount = preview?.actions.filter((item) => pendingActions.has(item.action)).length ?? 0;
	const selectedMode = modes.find((item) => item.value === mode) ?? modes[0];
	const rollbackEligible = run?.status === 'applied' && !run.rolled_back_at;
	const previewConsumed = !!preview && run?.preview_id === preview.id;

	const invalidatePlan = () => {
		setPreview(null);
		setRun(null);
		setFilter('all');
		setApplyOpen(false);
		setApplyConfirmed(false);
		setNotice('');
	};

	const readFile = async (event: ChangeEvent<HTMLInputElement>) => {
		const file = event.target.files?.[0];
		if (!file) return;
		setError('');
		invalidatePlan();
		try {
			const parsed = JSON.parse(await file.text()) as unknown;
			const nextBundle = portableBundleFromJSON(parsed);
			setBundle(nextBundle);
			setFileName(file.name);
		} catch (caught) {
			setBundle(null);
			setFileName('');
			setError(caught instanceof SyntaxError ? '文件不是有效的 JSON，请修正后重新选择。' : getErrorMessage(caught));
		} finally {
			event.target.value = '';
		}
	};

	const changeMode = (next: ConfigImportMode) => {
		if (next === mode) return;
		setMode(next);
		setError('');
		invalidatePlan();
	};

	const exportConfig = async () => {
		setBusy('export');
		setError('');
		setNotice('');
		try {
			const result = await api.exportPortableConfig();
			const blob = new Blob([JSON.stringify(result.data, null, 2)], { type: 'application/json;charset=utf-8' });
			const url = URL.createObjectURL(blob);
			const link = document.createElement('a');
			link.href = url;
			link.download = `relayops-config-${new Date().toISOString().slice(0, 10)}.json`;
			document.body.appendChild(link);
			link.click();
			link.remove();
			URL.revokeObjectURL(url);
			setNotice(`配置已从服务端导出，配置哈希 ${shortHash(result.data.config_hash)}。`);
		} catch (caught) {
			setError(getErrorMessage(caught));
		} finally {
			setBusy('');
		}
	};

	const createPreview = async () => {
		if (!bundle) return;
		setBusy('preview');
		setError('');
		setNotice('');
		setRun(null);
		try {
			const result = await api.previewConfigImport(mode, bundle);
			setPreview(result.data);
			setFilter('all');
		} catch (caught) {
			setPreview(null);
			setError(getErrorMessage(caught));
		} finally {
			setBusy('');
		}
	};

	const applyImport = async () => {
		if (!preview || !applyConfirmed) return;
		setBusy('apply');
		setError('');
		try {
			const result = await api.applyConfigImport(preview.id, preview.preview_hash);
			setRun(result.data);
			setRunId(String(result.data.id));
			setApplyOpen(false);
			setApplyConfirmed(false);
			setNotice(`导入执行记录 #${result.data.id} 已由服务端确认。`);
		} catch (caught) {
			setError(getErrorMessage(caught));
		} finally {
			setBusy('');
		}
	};

	const readRun = async (requestedId = runId) => {
		const normalized = requestedId.trim();
		if (!/^\d+$/.test(normalized) || Number(normalized) < 1) {
			setError('请输入有效的执行记录 ID。');
			return;
		}
		setBusy('read');
		setError('');
		setNotice('');
		try {
			const result = await api.getConfigImportRun(normalized);
			setRun(result.data);
			setRunId(String(result.data.id));
		} catch (caught) {
			setError(getErrorMessage(caught));
		} finally {
			setBusy('');
		}
	};

	const rollbackImport = async () => {
		if (!run || !rollbackEligible || !rollbackConfirmed) return;
		setBusy('rollback');
		setError('');
		try {
			const result = await api.rollbackConfigImport(run.id);
			setRun(result.data);
			setRollbackOpen(false);
			setRollbackConfirmed(false);
			setNotice(`执行记录 #${result.data.id} 已完成条件回滚。`);
		} catch (caught) {
			setError(getErrorMessage(caught));
		} finally {
			setBusy('');
		}
	};

	return <>
		<PageHeader eyebrow="Configuration portability" title="配置迁移" description="导出非敏感配置，在另一实例预览对象级变更，再以哈希保护执行或条件回滚。凭据密钥不会进入导出文件。" actions={<Button variant="secondary" onClick={() => void exportConfig()} disabled={busy !== ''}>{busy === 'export' ? <LoaderCircle size={16} className="spin" /> : <Download size={16} />}{busy === 'export' ? '正在导出' : '导出当前配置'}</Button>} />

		<div className="portability-assurance"><ShieldCheck size={22} /><div><strong>迁移文件只携带配置与凭据占位符</strong><span>导入前必须先生成冻结预览；执行时服务端会校验预览哈希和当前配置哈希，防止覆盖预览后发生的新修改。</span></div></div>

		{error && <div className="form-error portability-message" role="alert">{error}</div>}
		{notice && <div className="portability-notice" role="status"><Check size={17} />{notice}</div>}

		<div className="portability-workspace">
			<section className="portability-panel">
				<header><div><span>01</span><div><h2>选择导入文件</h2><p>支持本站导出的完整 JSON，也支持直接选择其中的 bundle。</p></div></div></header>
				<div className="portability-panel-body">
					<label className={`config-upload-zone ${bundle ? 'has-file' : ''}`}>
						<input type="file" accept="application/json,.json" onChange={(event) => void readFile(event)} />
						{bundle ? <Check size={25} /> : <Upload size={25} />}
						<strong>{fileName || '选择 RelayOps 配置 JSON'}</strong>
						<span>{bundle ? `${bundle.schema_version} · ${bundleCount(bundle)} 个可迁移对象` : '选择后只在浏览器解析，确认预览时才提交服务端'}</span>
					</label>
					<div className="config-mode-heading"><strong>冲突处理模式</strong><span>修改模式会使已有预览立即失效</span></div>
					<div className="config-mode-options" role="radiogroup" aria-label="冲突处理模式">{modes.map((item) => <button type="button" role="radio" aria-checked={mode === item.value} className={mode === item.value ? 'active' : ''} key={item.value} onClick={() => changeMode(item.value)}><span>{mode === item.value && <Check size={13} />}{item.label}</span><small>{item.description}</small></button>)}</div>
					<Button className="config-preview-button" onClick={() => void createPreview()} disabled={!bundle || busy !== ''}>{busy === 'preview' ? <LoaderCircle size={16} className="spin" /> : <FileJson size={16} />}{busy === 'preview' ? '服务端正在核对' : preview ? '重新生成预览' : '生成只读预览'}</Button>
				</div>
			</section>

			<section className="portability-panel">
				<header><div><span>记录</span><div><h2>读取执行记录</h2><p>输入历史 ID 可查看结果、待补对象与当前回滚资格。</p></div></div></header>
				<div className="portability-panel-body run-lookup-body">
					<label className="field"><span>执行记录 ID</span><div className="config-run-lookup"><input inputMode="numeric" pattern="[0-9]*" value={runId} onChange={(event) => setRunId(event.target.value)} placeholder="例如 12" onKeyDown={(event) => { if (event.key === 'Enter') void readRun(); }} /><Button variant="secondary" onClick={() => void readRun()} disabled={busy !== '' || !runId.trim()}>{busy === 'read' ? <LoaderCircle size={15} className="spin" /> : <History size={15} />}读取</Button></div></label>
					<div className="config-run-guidance"><CircleHelp size={18} /><span>执行成功后页面会自动填入 ID。再次打开页面时，也可以在这里读回服务端保存的真实记录。</span></div>
				</div>
			</section>
		</div>

		{preview && <Section title={`冻结预览 #${preview.id}`} description={`${selectedMode.label} · 创建于 ${formatTime(preview.created_at)} · 到期 ${formatTime(preview.expires_at)}`} action={<Button onClick={() => { setError(''); setApplyConfirmed(false); setApplyOpen(true); }} disabled={busy !== '' || previewConsumed}>{previewConsumed ? <Check size={16} /> : <ShieldCheck size={16} />}{previewConsumed ? '该预览已执行' : '核对并执行'}</Button>} className="config-preview-section">
			<SummaryStrip summary={preview.summary} />
			<div className="config-hash-row"><span>预览哈希 <code title={preview.preview_hash}>{shortHash(preview.preview_hash)}</code></span><span>预期配置 <code title={preview.expected_config_hash}>{shortHash(preview.expected_config_hash)}</code></span><span>文件哈希 <code title={preview.bundle_hash}>{shortHash(preview.bundle_hash)}</code></span></div>
			{preview.warnings.length > 0 && <div className="config-warning-list"><AlertTriangle size={17} /><div>{preview.warnings.map((warning) => <span key={warning}>{warning}</span>)}</div></div>}
			<div className="config-action-toolbar"><div><strong>对象级执行计划</strong><span>逐项展示最终动作、冲突与待补配置</span></div><div className="segmented" role="tablist" aria-label="预览对象筛选"><button role="tab" aria-selected={filter === 'all'} className={filter === 'all' ? 'active' : ''} onClick={() => setFilter('all')}>全部 {preview.actions.length}</button><button role="tab" aria-selected={filter === 'conflict'} className={filter === 'conflict' ? 'active' : ''} onClick={() => setFilter('conflict')}>冲突 {conflictCount}</button><button role="tab" aria-selected={filter === 'pending'} className={filter === 'pending' ? 'active' : ''} onClick={() => setFilter('pending')}>待补 {pendingCount}</button></div></div>
			<ActionRows actions={filteredActions} />
		</Section>}

		{run && <Section title={`执行记录 #${run.id}`} description={`由预览 #${run.preview_id} 产生 · ${formatTime(run.created_at)}`} action={<div className="config-run-actions"><Button variant="secondary" onClick={() => void readRun(String(run.id))} disabled={busy !== ''}>{busy === 'read' ? <LoaderCircle size={15} className="spin" /> : <RefreshCw size={15} />}刷新</Button>{rollbackEligible && <Button variant="danger" onClick={() => { setError(''); setRollbackConfirmed(false); setRollbackOpen(true); }} disabled={busy !== ''}><ArchiveRestore size={15} />条件回滚</Button>}</div>} className="config-run-section">
			<div className="config-run-head"><StatusBadge status={runStatus(run).health} label={runStatus(run).label} /><span>版本 {run.before_version_id} → {run.version_id}</span>{run.rolled_back_at && <span>回滚于 {formatTime(run.rolled_back_at)}</span>}</div>
			<SummaryStrip summary={run.summary} />
			<div className="config-hash-row"><span>执行前 <code title={run.before_hash}>{shortHash(run.before_hash)}</code></span><span>执行后 <code title={run.after_hash}>{shortHash(run.after_hash)}</code></span></div>
			{run.warnings.length > 0 && <div className="config-warning-list"><AlertTriangle size={17} /><div>{run.warnings.map((warning) => <span key={warning}>{warning}</span>)}</div></div>}
			<div className="config-pending-block"><div><strong>待补配置</strong><span>{run.pending_objects.length > 0 ? '这些对象已安全保持停用，补齐本地引用后再启用。' : '本次执行没有遗留待补对象。'}</span></div>{run.pending_objects.length > 0 ? <div className="config-pending-list">{run.pending_objects.map((item, index) => <div key={`${item.object_type}-${item.object_key}-${index}`}><span>{objectLabels[item.object_type] ?? item.object_type}</span><strong>{item.object_key}</strong><small>{item.reason}</small></div>)}</div> : <div className="config-action-empty compact"><Check size={17} />全部对象均已解析</div>}</div>
			<details className="config-run-details"><summary>查看本次 {run.actions.length} 个对象动作</summary><ActionRows actions={run.actions} /></details>
		</Section>}

		<Modal open={applyOpen} onClose={() => { if (busy !== 'apply') setApplyOpen(false); }} title="确认执行配置导入" description="服务端将再次校验预览与当前配置。校验失败时不会写入。" footer={<><Button variant="ghost" onClick={() => setApplyOpen(false)} disabled={busy === 'apply'}>取消</Button><Button onClick={() => void applyImport()} disabled={!applyConfirmed || busy === 'apply'}>{busy === 'apply' ? <LoaderCircle size={16} className="spin" /> : <ShieldCheck size={16} />}{busy === 'apply' ? '正在执行' : '确认并执行'}</Button></>}>
			{preview && <div className="config-confirmation"><div><span>处理模式</span><strong>{selectedMode.label}</strong></div><div><span>对象动作</span><strong>{preview.actions.length} 项</strong></div><div><span>冲突 / 待补</span><strong>{conflictCount} / {pendingCount}</strong></div><div className="full"><span>预览哈希</span><code>{preview.preview_hash}</code></div>{preview.warnings.length > 0 && <div className="config-warning-list full"><AlertTriangle size={17} /><div>{preview.warnings.map((warning) => <span key={warning}>{warning}</span>)}</div></div>}<label className="check-row full"><input type="checkbox" checked={applyConfirmed} onChange={(event) => setApplyConfirmed(event.target.checked)} /><span>我已核对对象动作、冲突和待补配置，并确认按此冻结预览执行</span></label></div>}
			{error && <div className="form-error" role="alert">{error}</div>}
		</Modal>

		<Modal open={rollbackOpen} onClose={() => { if (busy !== 'rollback') setRollbackOpen(false); }} title="确认条件回滚" description="只有当前配置仍与该次导入的执行后哈希一致，服务端才会恢复执行前版本。" footer={<><Button variant="ghost" onClick={() => setRollbackOpen(false)} disabled={busy === 'rollback'}>取消</Button><Button variant="danger" onClick={() => void rollbackImport()} disabled={!rollbackConfirmed || busy === 'rollback'}>{busy === 'rollback' ? <LoaderCircle size={16} className="spin" /> : <ArchiveRestore size={16} />}{busy === 'rollback' ? '正在回滚' : '确认回滚'}</Button></>}>
			{run && <div className="config-confirmation"><div><span>执行记录</span><strong>#{run.id}</strong></div><div><span>当前状态</span><strong>{run.status}</strong></div><div className="full"><span>必须匹配的执行后哈希</span><code>{run.after_hash}</code></div><label className="check-row full"><input type="checkbox" checked={rollbackConfirmed} onChange={(event) => setRollbackConfirmed(event.target.checked)} /><span>我理解若配置已被其他操作修改，服务端会拒绝回滚并保留现状</span></label></div>}
			{error && <div className="form-error" role="alert">{error}</div>}
		</Modal>
	</>;
}
