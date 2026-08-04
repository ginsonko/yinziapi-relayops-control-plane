import { useEffect, useMemo, useState } from 'react';
import { ArrowDown, ArrowDownRight, ArrowUp, ArrowUpRight, Pencil, Pin, Plus, RefreshCw, Search, SlidersHorizontal, Sparkles, Target, Trash2 } from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { CatalogSnapshot, Supplier, WatchItem, WatchItemInput, WatchSettings } from '../api/types';
import { Button, EmptyState, formatDateTime, IconButton, Modal, PageHeader, StatusBadge, Toggle } from '../components/ui';

type Filter = 'all' | 'manual' | 'automatic' | 'risk';

const defaultSettings: WatchSettings = { auto_enabled: true, retention_days: 7, automatic_limit: 30, overview_limit: 5 };

function manualID(item: WatchItem) {
	if (!item.id.startsWith('manual:')) return 0;
	return Number(item.id.slice('manual:'.length));
}

function watchInput(item: WatchItem): WatchItemInput {
	return {
		supplier_id: item.supplierId,
		scope_type: item.scopeType,
		group_key: item.groupKey,
		model_key: item.modelKey,
		display_name: item.displayName,
		note: item.note ?? '',
		evidence_hash: item.evidenceHash,
	};
}

export function WatchlistPage({ suppliers, onChanged, onNotify }: {
	suppliers: Supplier[];
	onChanged: () => Promise<void>;
	onNotify: (message: string, tone?: 'success' | 'info' | 'error') => void;
}) {
	const [items, setItems] = useState<WatchItem[]>([]);
	const [settings, setSettings] = useState<WatchSettings>(defaultSettings);
	const [candidateCount, setCandidateCount] = useState(0);
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState('');
	const [query, setQuery] = useState('');
	const [filter, setFilter] = useState<Filter>('all');
	const [editorOpen, setEditorOpen] = useState(false);
	const [settingsOpen, setSettingsOpen] = useState(false);
	const [editing, setEditing] = useState<WatchItem | null>(null);
	const [catalog, setCatalog] = useState<CatalogSnapshot | null>(null);
	const [catalogLoading, setCatalogLoading] = useState(false);
	const [saving, setSaving] = useState(false);
	const [form, setForm] = useState<WatchItemInput>({ supplier_id: Number(suppliers[0]?.id ?? 0), scope_type: 'group', group_key: '', model_key: '', display_name: '', note: '' });

	const load = async () => {
		setError('');
		try {
			const result = await api.watchlist();
			setItems(result.data.items);
			setSettings(result.data.settings);
			setCandidateCount(result.data.automatic_candidates);
		} catch (cause) {
			setError(getErrorMessage(cause));
		} finally {
			setLoading(false);
		}
	};

	useEffect(() => { void load(); }, []);

	const loadCatalog = async (supplierId: number) => {
		if (!supplierId) { setCatalog(null); return; }
		setCatalogLoading(true);
		try {
			const result = await api.supplierSnapshot(String(supplierId));
			setCatalog(result.data);
			setForm((current) => {
				if (current.supplier_id !== supplierId) return current;
				if (current.scope_type === 'group') {
					const selected = result.data.groups.some((group) => group.external_id === current.group_key || group.name === current.group_key);
					return selected ? current : { ...current, group_key: result.data.groups[0]?.external_id ?? '', model_key: '' };
				}
				const selected = result.data.models.some((model) => model.external_id === current.model_key || model.name === current.model_key);
				const model = selected ? result.data.models.find((entry) => entry.external_id === current.model_key || entry.name === current.model_key) : result.data.models[0];
				return { ...current, model_key: model?.external_id ?? '', group_key: model?.group ?? '' };
			});
		} catch {
			setCatalog(null);
		} finally {
			setCatalogLoading(false);
		}
	};

	const openCreate = () => {
		const supplierId = Number(suppliers[0]?.id ?? 0);
		setEditing(null);
		setForm({ supplier_id: supplierId, scope_type: 'group', group_key: '', model_key: '', display_name: '', note: '' });
		setEditorOpen(true);
		void loadCatalog(supplierId);
	};

	const openEdit = (item: WatchItem) => {
		setEditing(item);
		setForm(watchInput(item));
		setEditorOpen(true);
		void loadCatalog(item.supplierId);
	};

	const filtered = useMemo(() => {
		const normalized = query.trim().toLowerCase();
		return items.filter((item) => {
			if (filter === 'manual' && !item.manual) return false;
			if (filter === 'automatic' && item.manual) return false;
			if (filter === 'risk' && item.status === 'healthy' && (item.grossMargin === null || item.grossMargin >= 10)) return false;
			return !normalized || `${item.displayName} ${item.model} ${item.group} ${item.supplier} ${item.note ?? ''}`.toLowerCase().includes(normalized);
		});
	}, [filter, items, query]);

	const manualItems = items.filter((item) => item.manual);
	const moveManual = async (item: WatchItem, direction: -1 | 1) => {
		const index = manualItems.findIndex((entry) => entry.id === item.id);
		const target = index + direction;
		if (index < 0 || target < 0 || target >= manualItems.length) return;
		const reordered = [...manualItems];
		[reordered[index], reordered[target]] = [reordered[target], reordered[index]];
		try {
			await api.reorderWatchItems(reordered.map(manualID));
			await load();
			onNotify('手动关注顺序已更新', 'info');
		} catch (cause) { onNotify(getErrorMessage(cause), 'error'); }
	};

	const remove = async (item: WatchItem) => {
		try {
			await api.deleteWatchItem(item.id, watchInput(item));
			await load();
			await onChanged();
			onNotify(item.manual ? '手动关注已删除' : '已忽略当前自动关注证据', 'info');
		} catch (cause) { onNotify(getErrorMessage(cause), 'error'); }
	};

	const saveItem = async () => {
		setSaving(true); setError('');
		try {
			const input = { ...form, display_name: form.display_name.trim(), note: form.note.trim() };
			if (editing) await api.updateWatchItem(editing.id, input);
			else await api.createWatchItem(input);
			setEditorOpen(false);
			await load();
			await onChanged();
			onNotify(editing ? '关注项已更新' : '关注项已添加');
		} catch (cause) { setError(getErrorMessage(cause)); } finally { setSaving(false); }
	};

	const saveSettings = async () => {
		setSaving(true); setError('');
		try {
			const result = await api.updateWatchSettings(settings);
			setSettings(result.data);
			setSettingsOpen(false);
			await load();
			await onChanged();
			onNotify('自动关注设置已保存');
		} catch (cause) { setError(getErrorMessage(cause)); } finally { setSaving(false); }
	};

	return <>
		<PageHeader eyebrow="经营监控" title="重点关注" description="手动固定长期对象，自动收拢正在使用的规则和近期真实变化。关注操作不会触发采集或改价。" actions={<>
			<Button variant="secondary" onClick={() => { setError(''); setSettingsOpen(true); }}><SlidersHorizontal size={16} />自动关注</Button>
			<Button onClick={openCreate}><Plus size={16} />添加关注</Button>
		</>} />
		<div className="watch-summary">
			<div><span>当前展示</span><strong>{items.length}</strong><small>{manualItems.length} 项手动永久保留</small></div>
			<div><span>自动候选</span><strong>{candidateCount}</strong><small>最多展示 {settings.automatic_limit} 项</small></div>
			<div><span>近期变化</span><strong>{items.filter((item) => item.sourceChange).length}</strong><small>仅统计可信语义变化</small></div>
		</div>
		<div className="toolbar watch-toolbar">
			<div className="toolbar-search"><Search size={17} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索上游、分组、模型或备注" /></div>
			<div className="segmented" role="tablist" aria-label="关注来源筛选">
				{([['all', '全部'], ['manual', '手动'], ['automatic', '自动'], ['risk', '风险']] as const).map(([value, label]) => <button key={value} type="button" className={filter === value ? 'active' : ''} onClick={() => setFilter(value)}>{label}</button>)}
			</div>
		</div>
		{error && !editorOpen && !settingsOpen && <div className="form-error">{error}</div>}
		{loading ? <div className="inline-loading"><RefreshCw className="spin" size={18} />读取重点关注…</div> : filtered.length === 0 ? <EmptyState icon={<Target size={24} />} title="当前筛选下没有关注项" description="可添加手动关注；启用规则或出现可信变化后，自动项会自行进入。" /> : <div className="watch-grid">
			{filtered.map((item) => <article className="watch-card" key={item.id}>
				<div className="watch-card-head"><div><strong>{item.displayName || item.model}</strong><span>{item.supplier} · {item.group}</span></div><StatusBadge status={item.status} /></div>
				<div className="watch-source-row">{item.manual && <span><Pin size={12} />手动</span>}{item.sourcePricing && <span>价格映射</span>}{item.sourceMirror && <span>分组镜像</span>}{item.sourceChange && <span><Sparkles size={12} />近期变化</span>}</div>
				<div className="rate-comparison"><div><span>上游倍率</span><strong>{item.upstreamRate ?? '待获取'}</strong></div><div><span>本站倍率</span><strong>{item.localRate ?? (item.sourceMirror && !item.sourcePricing ? '仅镜像模型' : '待映射')}</strong></div></div>
				<div className="watch-card-foot">
					<span>规则价差 <strong className={item.grossMargin === null ? '' : item.grossMargin < 0 ? 'negative' : item.grossMargin < 12 ? 'warning' : 'positive'}>{item.sourcePricing ? item.grossMargin === null ? '待计算' : `${item.grossMargin.toFixed(1)}%` : '不适用'}</strong></span>
					<span>24h {item.delta24h === null ? <strong>样本不足</strong> : <>{item.delta24h > 0 ? <ArrowUpRight size={14} /> : item.delta24h < 0 ? <ArrowDownRight size={14} /> : null}<strong className={item.delta24h > 0 ? 'negative' : item.delta24h < 0 ? 'positive' : ''}>{item.delta24h > 0 ? '+' : ''}{item.delta24h.toFixed(2)}%</strong></>}</span>
				</div>
				{item.note && <p className="watch-note">{item.note}</p>}
				<div className="watch-actions"><small>更新于 {formatDateTime(item.updatedAt)}</small><div>{item.manual && <><IconButton label="上移" onClick={() => void moveManual(item, -1)}><ArrowUp size={15} /></IconButton><IconButton label="下移" onClick={() => void moveManual(item, 1)}><ArrowDown size={15} /></IconButton><IconButton label="编辑" onClick={() => openEdit(item)}><Pencil size={15} /></IconButton></>}<IconButton label={item.manual ? '删除手动关注' : '忽略当前自动关注'} onClick={() => void remove(item)}><Trash2 size={15} /></IconButton></div></div>
			</article>)}
		</div>}

		<Modal open={editorOpen} onClose={() => !saving && setEditorOpen(false)} title={editing ? '编辑关注项' : '添加关注项'} description="关注设置只改变监控视图，不会立即采集、修改规则或写入本站。" footer={<><Button variant="ghost" onClick={() => setEditorOpen(false)} disabled={saving}>取消</Button><Button onClick={() => void saveItem()} disabled={saving || !form.supplier_id || (form.scope_type === 'group' ? !form.group_key : !form.model_key)}>{saving && <RefreshCw className="spin" size={15} />}{editing ? '保存修改' : '添加关注'}</Button></>}>
			<div className="form-grid">
				<label className="field full"><span>上游站点</span><select value={form.supplier_id} onChange={(event) => { const supplierId = Number(event.target.value); setForm((current) => ({ ...current, supplier_id: supplierId, group_key: '', model_key: '' })); void loadCatalog(supplierId); }}>{suppliers.map((supplier) => <option key={supplier.id} value={supplier.id}>{supplier.name}</option>)}</select></label>
				<div className="field full"><span>关注类型</span><div className="segmented"><button type="button" className={form.scope_type === 'group' ? 'active' : ''} onClick={() => { setForm((current) => ({ ...current, scope_type: 'group', group_key: catalog?.groups[0]?.external_id ?? '', model_key: '' })); }}>分组</button><button type="button" className={form.scope_type === 'model' ? 'active' : ''} onClick={() => { const model = catalog?.models[0]; setForm((current) => ({ ...current, scope_type: 'model', model_key: model?.external_id ?? '', group_key: model?.group ?? '' })); }}>模型</button></div></div>
				{form.scope_type === 'group' ? <label className="field full"><span>上游分组</span><select value={form.group_key} disabled={catalogLoading} onChange={(event) => setForm((current) => ({ ...current, group_key: event.target.value }))}>{catalog?.groups.map((group) => <option key={group.external_id || group.name} value={group.external_id || group.name}>{group.name} · {group.model_ids.length} 个模型</option>)}</select></label> : <label className="field full"><span>上游模型</span><select value={form.model_key} disabled={catalogLoading} onChange={(event) => { const model = catalog?.models.find((entry) => entry.external_id === event.target.value); setForm((current) => ({ ...current, model_key: event.target.value, group_key: model?.group ?? '' })); }}>{catalog?.models.map((model) => <option key={model.external_id || model.name} value={model.external_id || model.name}>{model.name}{model.group ? ` · ${model.group}` : ''}</option>)}</select></label>}
				<label className="field full"><span>显示名称（可选）</span><input value={form.display_name} onChange={(event) => setForm((current) => ({ ...current, display_name: event.target.value }))} placeholder="留空时使用上游名称" /></label>
				<label className="field full"><span>备注（可选）</span><textarea value={form.note} onChange={(event) => setForm((current) => ({ ...current, note: event.target.value }))} rows={3} /></label>
			</div>
			{catalogLoading && <div className="inline-loading"><RefreshCw className="spin" size={16} />读取可信目录…</div>}
			{!catalogLoading && catalog && (form.scope_type === 'group' ? catalog.groups.length === 0 : catalog.models.length === 0) && <div className="form-error">当前可信目录没有可选对象；等待下一次正常采集后再添加。</div>}
			{error && <div className="form-error">{error}</div>}
		</Modal>

		<Modal open={settingsOpen} onClose={() => !saving && setSettingsOpen(false)} title="自动关注设置" description="自动关注只复用已经落库的规则与可信变化，不会增加采集频率。" footer={<><Button variant="ghost" onClick={() => setSettingsOpen(false)} disabled={saving}>取消</Button><Button onClick={() => void saveSettings()} disabled={saving}>{saving && <RefreshCw className="spin" size={15} />}保存设置</Button></>}>
			<div className="watch-settings">
				<label className="toggle-label"><Toggle checked={settings.auto_enabled} onChange={(value) => setSettings((current) => ({ ...current, auto_enabled: value }))} label="自动关注" /><span><strong>自动关注</strong><small>规则引用或近期真实变化自动进入。</small></span></label>
				<label className="field"><span>无规则对象保留天数</span><input type="number" min={1} max={90} value={settings.retention_days} onChange={(event) => setSettings((current) => ({ ...current, retention_days: Number(event.target.value) }))} /></label>
				<label className="field"><span>自动关注上限</span><input type="number" min={1} max={200} value={settings.automatic_limit} onChange={(event) => setSettings((current) => ({ ...current, automatic_limit: Number(event.target.value) }))} /></label>
				<label className="field"><span>总览展示数量</span><input type="number" min={1} max={20} value={settings.overview_limit} onChange={(event) => setSettings((current) => ({ ...current, overview_limit: Number(event.target.value) }))} /></label>
			</div>
			{error && <div className="form-error">{error}</div>}
		</Modal>
	</>;
}
