import { useMemo, useState } from 'react';
import { Activity, AlertTriangle, BadgePercent, Clock3, ExternalLink, KeyRound, ListChecks, MoreVertical, Pencil, Plus, RefreshCw, Search, Server, ShieldCheck, Star, Trash2 } from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { AuthCapabilityEvidence, CollectionAttempt, CreateSupplierInput, DeleteSupplierInput, RawSupplier, Supplier, SupplierDeletionImpact, UpdateSupplierInput } from '../api/types';
import { Button, EmptyState, formatMoney, Modal, PageHeader, StatusBadge } from '../components/ui';
import { normalizeUpstreamKeyManagementPath } from './upstreamKeyManagement';

type SupplierPageProps = {
	suppliers: Supplier[];
	collectingId: string | null;
	onCollect: (id: string) => void;
	onAdd: (input: CreateSupplierInput) => Promise<void>;
	onEdit: (supplierId: string, input: UpdateSupplierInput) => Promise<void>;
	onDelete: (supplierId: string, input: DeleteSupplierInput) => Promise<void>;
	onSaveRechargeFactor: (supplierId: string, factor: string) => Promise<void>;
	onOpenCredentials: (supplierId: string, action: 'login' | 'renewal') => void;
};

type SupplierEditForm = {
	raw: RawSupplier;
	name: string;
	url: string;
	adapter: string;
	interval: number;
	factor: string;
	enabled: boolean;
	pricingPath: string;
	balancePath: string;
	apiKeyManagementPath: string;
};

export function SuppliersPage({ suppliers, collectingId, onCollect, onAdd, onEdit, onDelete, onSaveRechargeFactor, onOpenCredentials }: SupplierPageProps) {
  const [query, setQuery] = useState('');
  const [status, setStatus] = useState('all');
  const [addOpen, setAddOpen] = useState(false);
  const [form, setForm] = useState({ name: '', url: '', kind: 'NewAPI' as Supplier['kind'], interval: 60, factor: '1' });
  const [saving, setSaving] = useState(false);
  const [formError, setFormError] = useState('');
	const [evidenceSupplier, setEvidenceSupplier] = useState<Supplier | null>(null);
	const [attempts, setAttempts] = useState<CollectionAttempt[]>([]);
	const [capabilities, setCapabilities] = useState<AuthCapabilityEvidence[]>([]);
	const [evidenceLoading, setEvidenceLoading] = useState(false);
	const [evidenceError, setEvidenceError] = useState('');
	const [factorSupplier, setFactorSupplier] = useState<Supplier | null>(null);
	const [factorValue, setFactorValue] = useState('1');
	const [factorSaving, setFactorSaving] = useState(false);
	const [factorError, setFactorError] = useState('');
	const [editSummary, setEditSummary] = useState<Supplier | null>(null);
	const [editForm, setEditForm] = useState<SupplierEditForm | null>(null);
	const [editLoading, setEditLoading] = useState(false);
	const [editSaving, setEditSaving] = useState(false);
	const [editError, setEditError] = useState('');
	const [deleteSummary, setDeleteSummary] = useState<Supplier | null>(null);
	const [deleteImpact, setDeleteImpact] = useState<SupplierDeletionImpact | null>(null);
	const [deleteLoading, setDeleteLoading] = useState(false);
	const [deleteSaving, setDeleteSaving] = useState(false);
	const [deleteConfirmation, setDeleteConfirmation] = useState('');
	const [deleteError, setDeleteError] = useState('');
  const filtered = useMemo(() => suppliers.filter((supplier) => {
    const matchesQuery = `${supplier.name} ${supplier.baseUrl} ${supplier.kind}`.toLowerCase().includes(query.toLowerCase());
    return matchesQuery && (status === 'all' || supplier.status === status);
  }), [query, status, suppliers]);

  const submit = async () => {
    if (!form.name.trim() || !form.url.trim()) return;
    setSaving(true);
    setFormError('');
    const adapter: CreateSupplierInput['adapter'] = form.kind === 'NewAPI' ? 'newapi' : form.kind === 'Sub2API' ? 'sub2api' : 'json';
    try {
      await onAdd({
      name: form.name.trim(),
        base_url: form.url.trim(),
        adapter,
        poll_interval_seconds: form.interval,
		recharge_value_factor: form.factor.trim() || '1',
        collect_now: true,
        enabled: true,
        focus_groups: [],
        focus_models: [],
		metadata: {},
      });
      setAddOpen(false);
      setForm({ name: '', url: '', kind: 'NewAPI', interval: 60, factor: '1' });
    } catch (error) {
      setFormError(getErrorMessage(error));
    } finally {
      setSaving(false);
    }
  };

	const openFactor = (supplier: Supplier) => {
		setFactorSupplier(supplier);
		setFactorValue(supplier.rechargeValueFactor || '1');
		setFactorError('');
	};

	const saveFactor = async () => {
		if (!factorSupplier) return;
		const parsed = Number(factorValue);
		if (!Number.isFinite(parsed) || parsed <= 0) {
			setFactorError('折算系数必须是大于 0 的十进制数。');
			return;
		}
		setFactorSaving(true);
		setFactorError('');
		try {
			await onSaveRechargeFactor(factorSupplier.id, factorValue.trim());
			setFactorSupplier(null);
		} catch (error) {
			setFactorError(getErrorMessage(error));
		} finally {
			setFactorSaving(false);
		}
	};

	const openEdit = async (supplier: Supplier) => {
		setEditSummary(supplier);
		setEditForm(null);
		setEditError('');
		setEditLoading(true);
		try {
			const response = await api.listSuppliers();
			const raw = response.data.find((item) => String(item.id) === supplier.id);
			if (!raw) throw new Error('没有找到对应上游，请刷新页面后重试。');
			setEditForm({
				raw, name: raw.name, url: raw.base_url, adapter: raw.adapter,
				interval: raw.poll_interval_seconds, factor: raw.recharge_value_factor || '1', enabled: raw.enabled,
				pricingPath: raw.pricing_path ?? '', balancePath: raw.balance_path ?? '',
				apiKeyManagementPath: raw.metadata?.api_key_management_path ?? '',
			});
		} catch (error) {
			setEditError(getErrorMessage(error));
		} finally {
			setEditLoading(false);
		}
	};

	const saveEdit = async () => {
		if (!editForm || !editSummary) return;
		if (!editForm.name.trim() || !editForm.url.trim()) {
			setEditError('站点名称和地址不能为空。');
			return;
		}
		if (!Number.isInteger(editForm.interval) || editForm.interval < 30 || editForm.interval > 86400) {
			setEditError('采集间隔必须在 30 秒到 24 小时之间。');
			return;
		}
		const parsedFactor = Number(editForm.factor);
		if (!Number.isFinite(parsedFactor) || parsedFactor <= 0) {
			setEditError('充值折算系数必须是大于 0 的十进制数。');
			return;
		}
		const keyManagementPath = editForm.apiKeyManagementPath.trim();
		const normalizedKeyManagementPath = normalizeUpstreamKeyManagementPath(keyManagementPath);
		if (normalizedKeyManagementPath === null) {
			setEditError('API Key 管理页必须是规范的同站路径，例如 /keys；不能包含域名、重复斜线、点段、查询参数、片段或控制字符。');
			return;
		}
		setEditSaving(true);
		setEditError('');
		try {
			const metadata = { ...(editForm.raw.metadata ?? {}) };
			if (normalizedKeyManagementPath) metadata.api_key_management_path = normalizedKeyManagementPath;
			else delete metadata.api_key_management_path;
			await onEdit(editSummary.id, {
				name: editForm.name.trim(), base_url: editForm.url.trim(), adapter: editForm.adapter,
				pricing_path: editForm.pricingPath.trim(), balance_path: editForm.balancePath.trim(),
				credential_id: editForm.raw.credential_id ?? null, enabled: editForm.enabled,
				poll_interval_seconds: editForm.interval, recharge_value_factor: editForm.factor.trim(),
				focus_groups: editForm.raw.focus_groups ?? [], focus_models: editForm.raw.focus_models ?? [],
				metadata,
			});
			setEditSummary(null);
			setEditForm(null);
		} catch (error) {
			setEditError(getErrorMessage(error));
		} finally {
			setEditSaving(false);
		}
	};

	const loadDeletionImpact = async (supplier: Supplier) => {
		setDeleteLoading(true);
		setDeleteImpact(null);
		try {
			const response = await api.supplierDeletionImpact(supplier.id);
			setDeleteImpact(response.data);
		} catch (error) {
			setDeleteError(getErrorMessage(error));
		} finally {
			setDeleteLoading(false);
		}
	};

	const openDelete = (supplier: Supplier) => {
		setDeleteSummary(supplier);
		setDeleteConfirmation('');
		setDeleteError('');
		void loadDeletionImpact(supplier);
	};

	const confirmDelete = async () => {
		if (!deleteSummary || !deleteImpact) return;
		setDeleteSaving(true);
		setDeleteError('');
		try {
			await onDelete(deleteSummary.id, {
				confirm_name: deleteConfirmation.trim(), impact_hash: deleteImpact.impact_hash,
			});
			setDeleteSummary(null);
			setDeleteImpact(null);
			setDeleteConfirmation('');
		} catch (error) {
			setDeleteError(getErrorMessage(error));
			setDeleteConfirmation('');
			await loadDeletionImpact(deleteSummary);
		} finally {
			setDeleteSaving(false);
		}
	};

	const openEvidence = async (supplier: Supplier) => {
		setEvidenceSupplier(supplier);
		setEvidenceLoading(true);
		setEvidenceError('');
		setAttempts([]);
		setCapabilities([]);
		try {
			const [attemptResult, capabilityResult] = await Promise.all([
				api.supplierAttempts(supplier.id, 20), api.supplierCapabilities(supplier.id),
			]);
			setAttempts(attemptResult.data);
			setCapabilities(capabilityResult.data);
		} catch (error) {
			setEvidenceError(getErrorMessage(error));
		} finally {
			setEvidenceLoading(false);
		}
	};

	const formatTimestamp = (value?: string) => {
		if (!value) return '尚无记录';
		const date = new Date(value);
		return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false });
	};
	const capabilityLabels: Record<string, string> = {
		public_catalog: '公开目录', anonymous: '匿名访问', customer_session: '客户会话', business_api_key: '业务 API Key', site_admin: '站点管理员',
	};
	const capabilityName = (value: string) => capabilityLabels[value] ?? value;
	const supplierSummaryNote = (supplier: Supplier, catalogAvailable: boolean) => {
		if (supplier.authAttention && catalogAvailable) {
			const missing = supplier.authAttentionReasons?.length ? supplier.authAttentionReasons.join('、') : '部分私有能力';
			return `目录快照仍可用；最新证据显示${missing}需要补充授权，详情见采集证据。`;
		}
		switch (supplier.lastErrorClass) {
			case 'auth_missing':
			case 'auth_expired':
				return '当前授权不足，尚无法读取私有目录；请登录、续期或配置站点支持的 API Key。';
			case 'permission_denied':
				return '当前凭据权限不足；请重新授权具备只读目录和余额权限的账号。';
			case 'connection_reset':
			case 'read_timeout':
			case 'timeout':
				return '上游暂时无法连接；系统会按计划自动重试，并保留最近可信数据。';
			case 'contract_changed':
				return '上游接口结构发生变化；请检查站点契约，最近可信数据继续保留。';
			default:
				return supplier.note ?? '';
		}
	};
	const deleteConfigurationCount = deleteImpact ? deleteImpact.pricing_policies + deleteImpact.change_proposals + deleteImpact.listing_plans + deleteImpact.listing_change_sets : 0;
	const deleteHistoryCount = deleteImpact ? deleteImpact.catalog_snapshots + deleteImpact.price_observations + deleteImpact.balance_observations + deleteImpact.discoveries : 0;
	const deleteEvidenceCount = deleteImpact ? deleteImpact.collection_attempts + deleteImpact.request_evidence + deleteImpact.auth_capabilities + deleteImpact.pairing_sessions + deleteImpact.manifest_bindings : 0;
	const deleteNameConfirmed = !!deleteImpact && deleteConfirmation.trim() === deleteImpact.supplier_name.trim();

  return (
    <>
      <PageHeader eyebrow="供应链" title="上游站点" description="集中维护采集目标、登录状态和价格目录。新增后可立即采集，不需要等待观察期。" actions={<Button onClick={() => setAddOpen(true)}><Plus size={17} />添加上游</Button>} />
      <div className="toolbar">
        <div className="toolbar-search"><Search size={17} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索站点名称、地址或类型" /></div>
        <div className="segmented" aria-label="状态筛选">
          {[['all', '全部'], ['healthy', '正常'], ['warning', '需关注'], ['error', '异常']].map(([value, label]) => <button key={value} className={status === value ? 'active' : ''} onClick={() => setStatus(value)}>{label}</button>)}
        </div>
      </div>

      {filtered.length === 0 ? (
        <EmptyState icon={<Server size={24} />} title="没有匹配的上游" description="调整筛选条件，或添加一个新的采购站点。" action={<Button onClick={() => setAddOpen(true)}><Plus size={17} />添加上游</Button>} />
      ) : (
        <div className="supplier-list">
		  {filtered.map((supplier) => {
			const catalogAvailable = supplier.groupCount > 0 || supplier.modelCount > 0;
			const showAuthAction = supplier.authStatus !== 'valid' || supplier.authAttention;
			const authAction = supplier.authStatus === 'missing' ? 'login' : 'renewal';
			const attentionLabel = supplier.authAttentionReasons?.length === 1 ? `补充${supplier.authAttentionReasons[0]}授权` : '补充授权';
			const summaryNote = supplierSummaryNote(supplier, catalogAvailable);
			return (
            <article className="supplier-row" key={supplier.id}>
              <div className={`supplier-logo status-${supplier.status}`}>{supplier.name.slice(0, 1)}</div>
              <div className="supplier-main">
                <div className="supplier-title"><strong>{supplier.name}</strong>{supplier.focused && <span className="focus-mark"><Star size={13} fill="currentColor" />重点</span>}<StatusBadge status={supplier.status} />{!supplier.enabled && <StatusBadge status="unknown" label="已暂停" />}</div>
                <a href={supplier.baseUrl} target="_blank" rel="noreferrer">{supplier.baseUrl}<ExternalLink size={12} /></a>
				{summaryNote && <p className={`inline-note ${supplier.status}`}>{summaryNote}</p>}
              </div>
              <div className="supplier-meta"><span>类型<strong>{supplier.kind}</strong></span><span>分组 / 模型<strong>{supplier.groupCount} / {supplier.modelCount}</strong></span><span>现金折算<strong>× {supplier.rechargeValueFactor || '1'}</strong></span><span>余额<strong>{formatMoney(supplier.balance, supplier.balanceCurrency)}</strong></span><span>成功耗时<strong>{supplier.latencyMs !== null ? `${supplier.latencyMs} ms` : '待采样'}</strong></span></div>
			  <div className="supplier-sync"><span>可信成功：{formatTimestamp(supplier.lastSuccessAt)}</span><small>最近尝试：{formatTimestamp(supplier.lastAttemptAt)} · 下次：{formatTimestamp(supplier.nextCollectedAt)}</small>{supplier.priceChangeCount > 0 && <b>{supplier.priceChangeCount} 项价格变化</b>}</div>
              <div className="supplier-actions">
				{showAuthAction && <Button variant="secondary" onClick={() => onOpenCredentials(supplier.id, authAction)}><KeyRound size={16} />{supplier.authAttention && supplier.authStatus === 'valid' ? attentionLabel : supplier.authStatus === 'missing' ? '登录' : '续期'}</Button>}
				<Button variant="secondary" onClick={() => void openEvidence(supplier)}><ListChecks size={16} />采集证据</Button>
				<Button variant="secondary" onClick={() => openFactor(supplier)}><BadgePercent size={16} />现金折算</Button>
                <Button variant="secondary" onClick={() => onCollect(supplier.id)} disabled={collectingId === supplier.id}><RefreshCw size={16} className={collectingId === supplier.id ? 'spin' : ''} />采集</Button>
				<details className="supplier-menu">
					<summary className="icon-button" aria-label={`管理 ${supplier.name}`} title="编辑或删除上游"><MoreVertical size={18} /></summary>
					<div role="menu">
						<button type="button" role="menuitem" onClick={(event) => { event.currentTarget.closest('details')?.removeAttribute('open'); void openEdit(supplier); }}><Pencil size={15} />编辑配置</button>
						<button type="button" role="menuitem" className="danger" onClick={(event) => { event.currentTarget.closest('details')?.removeAttribute('open'); openDelete(supplier); }}><Trash2 size={15} />删除上游</button>
					</div>
				</details>
              </div>
            </article>
			);
		  })}
        </div>
      )}

      <Modal open={addOpen} onClose={() => !saving && setAddOpen(false)} title="添加上游站点" description="系统会保存站点并立即完成第一次采集；登录后可见的价格可以随后配对凭据。" footer={<><Button variant="ghost" onClick={() => setAddOpen(false)} disabled={saving}>取消</Button><Button onClick={() => void submit()} disabled={saving || !form.name.trim() || !form.url.trim()}>{saving && <RefreshCw size={16} className="spin" />}{saving ? '正在保存并采集' : '保存并立即采集'}</Button></>}>
        <div className="form-grid">
          <label className="field full"><span>站点名称</span><input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="例如：主要 GPT 上游" /></label>
          <label className="field full"><span>站点地址</span><input value={form.url} onChange={(event) => setForm({ ...form, url: event.target.value })} placeholder="https://example.com" /></label>
          <label className="field"><span>连接器类型</span><select value={form.kind} onChange={(event) => setForm({ ...form, kind: event.target.value as Supplier['kind'] })}><option>NewAPI</option><option>Sub2API</option><option>JSON Connector</option></select></label>
          <label className="field"><span>采集间隔</span><select value={form.interval} onChange={(event) => setForm({ ...form, interval: Number(event.target.value) })}><option value={60}>每 1 分钟</option><option value={120}>每 2 分钟</option><option value={300}>每 5 分钟</option><option value={900}>每 15 分钟</option></select></label>
		  <label className="field"><span>充值折算系数</span><input inputMode="decimal" value={form.factor} onChange={(event) => setForm({ ...form, factor: event.target.value })} placeholder="默认 1" /></label>
        </div>
        {formError && <div className="form-error">{formError}</div>}
        <div className="info-callout"><KeyRound size={18} /><div><strong>登录后才能看到价格？</strong><span>先保存站点，再通过浏览器配对或 Access Token 授权。系统只保存加密凭据，不采集密码。</span></div></div>
      </Modal>

	  <Modal open={!!editSummary} onClose={() => { if (!editSaving) { setEditSummary(null); setEditForm(null); } }} title={`${editSummary?.name ?? ''} · 编辑上游`} description="保存后立即从服务端读回；凭据、重点关注范围和未展示的元数据会原样保留。" footer={<><Button variant="ghost" onClick={() => { setEditSummary(null); setEditForm(null); }} disabled={editSaving}>取消</Button><Button onClick={() => void saveEdit()} disabled={editLoading || editSaving || !editForm}>{editSaving && <RefreshCw size={16} className="spin" />}{editSaving ? '正在保存' : '保存修改'}</Button></>}>
		{editLoading ? <div className="inline-loading"><RefreshCw size={18} className="spin" />读取完整上游配置…</div> : editForm ? <div className="supplier-edit-workspace">
			<div className="form-grid">
				<label className="field full"><span>站点名称</span><input value={editForm.name} onChange={(event) => setEditForm({ ...editForm, name: event.target.value })} /></label>
				<label className="field full"><span>站点地址</span><input value={editForm.url} onChange={(event) => setEditForm({ ...editForm, url: event.target.value })} /></label>
				<label className="field"><span>连接器类型</span><select value={editForm.adapter} onChange={(event) => setEditForm({ ...editForm, adapter: event.target.value })}>{!['auto', 'newapi', 'sub2api', 'json'].includes(editForm.adapter) && <option value={editForm.adapter}>当前：{editForm.adapter}</option>}<option value="auto">自动识别</option><option value="newapi">NewAPI</option><option value="sub2api">Sub2API</option><option value="json">JSON Connector</option></select></label>
				<label className="field"><span>采集间隔（秒）</span><input type="number" min={30} max={86400} step={30} value={editForm.interval} onChange={(event) => setEditForm({ ...editForm, interval: Number(event.target.value) })} /></label>
				<label className="field"><span>充值折算系数</span><input inputMode="decimal" value={editForm.factor} onChange={(event) => setEditForm({ ...editForm, factor: event.target.value })} /></label>
				<label className="field supplier-enabled-field"><span>自动采集</span><span className="checkbox-row"><input type="checkbox" checked={editForm.enabled} onChange={(event) => setEditForm({ ...editForm, enabled: event.target.checked })} /><b>{editForm.enabled ? '已启用' : '已暂停'}</b></span></label>
			</div>
			<details className="advanced-fields"><summary>高级端点设置</summary><div className="form-grid"><label className="field full"><span>定价路径</span><input value={editForm.pricingPath} onChange={(event) => setEditForm({ ...editForm, pricingPath: event.target.value })} placeholder="留空时由连接器自动发现" /></label><label className="field full"><span>余额路径</span><input value={editForm.balancePath} onChange={(event) => setEditForm({ ...editForm, balancePath: event.target.value })} placeholder="留空时由连接器自动发现" /></label><label className="field full"><span>API Key 管理页路径</span><input value={editForm.apiKeyManagementPath} onChange={(event) => setEditForm({ ...editForm, apiKeyManagementPath: event.target.value })} placeholder="例如 /keys 或 /console/token" /><small>只允许当前上游站点内的相对路径。留空时，Sub2 家族提供 /keys，NewAPI 同时提供新版 /keys 和经典版 /console/token，自定义连接器回到站点首页；魔改站点可在此固定实际入口。</small></label></div></details>
			<div className="info-callout"><ShieldCheck size={18} /><div><strong>修改地址不会绕过凭据同源保护</strong><span>如果现有凭据属于另一个域名，服务端会拒绝保存；请先在凭据页重新授权，不会静默复用令牌。</span></div></div>
		</div> : null}
		{editError && <div className="form-error">{editError}</div>}
	  </Modal>

	  <Modal open={!!deleteSummary} onClose={() => { if (!deleteSaving) { setDeleteSummary(null); setDeleteImpact(null); setDeleteConfirmation(''); } }} title={`${deleteSummary?.name ?? ''} · 删除上游`} description="这是不可撤销的本地管理数据删除，不会修改 NewAPI，也不会向上游发送任何删除请求。" footer={<><Button variant="ghost" onClick={() => { setDeleteSummary(null); setDeleteImpact(null); setDeleteConfirmation(''); }} disabled={deleteSaving}>取消</Button><Button variant="danger" onClick={() => void confirmDelete()} disabled={deleteLoading || deleteSaving || !deleteImpact || !deleteNameConfirmed}>{deleteSaving && <RefreshCw size={16} className="spin" />}{deleteSaving ? '正在安全删除' : '确认删除'}</Button></>}>
		{deleteLoading ? <div className="inline-loading"><RefreshCw size={18} className="spin" />核对关联数据和删除边界…</div> : deleteImpact ? <div className="deletion-impact">
			<div className="danger-callout"><AlertTriangle size={20} /><div><strong>删除后无法在控制台恢复这些记录</strong><span>目标站点和线上 API 不受影响；该上游的自动采集与跟价规则会停止。</span></div></div>
			<div className="deletion-impact-grid">
				<div><span>规则与执行草案</span><strong>{deleteConfigurationCount}</strong><small>{deleteImpact.pricing_policies} 条定价规则 · {deleteImpact.change_proposals} 条变更提案 · {deleteImpact.listing_plans + deleteImpact.listing_change_sets} 条上架记录</small></div>
				<div><span>目录与价格历史</span><strong>{deleteHistoryCount}</strong><small>{deleteImpact.catalog_snapshots} 份目录 · {deleteImpact.price_observations} 条价格 · {deleteImpact.balance_observations} 条余额 · {deleteImpact.discoveries} 条发现</small></div>
				<div><span>采集与认证证据</span><strong>{deleteEvidenceCount}</strong><small>{deleteImpact.collection_attempts} 次采集 · {deleteImpact.request_evidence} 条请求证据 · {deleteImpact.auth_capabilities} 条能力记录</small></div>
				<div><span>活动与独立凭据</span><strong>{deleteImpact.credential_preserved ? '凭据保留' : '无凭据'}</strong><small>{deleteImpact.activities_detached} 条活动改为脱敏审计；不会删除任何独立凭据</small></div>
			</div>
			<label className="field full delete-confirmation"><span>输入站点名称 <b>{deleteImpact.supplier_name}</b> 以确认</span><input autoComplete="off" value={deleteConfirmation} onChange={(event) => setDeleteConfirmation(event.target.value)} placeholder={deleteImpact.supplier_name} /></label>
		</div> : null}
		{deleteError && <div className="form-error">{deleteError}</div>}
	  </Modal>

	  <Modal open={!!factorSupplier} onClose={() => !factorSaving && setFactorSupplier(null)} title={`${factorSupplier?.name ?? ''} · 充值折算`} description="把上游账面倍率换算成真实现金成本。默认 1；例如充值 1 元得到 2.5 元额度时填写 0.4。" footer={<><Button variant="ghost" onClick={() => setFactorSupplier(null)} disabled={factorSaving}>取消</Button><Button onClick={() => void saveFactor()} disabled={factorSaving || !factorValue.trim()}>{factorSaving && <RefreshCw size={16} className="spin" />}{factorSaving ? '正在重算' : '保存并实时重算'}</Button></>}>
		<div className="form-grid">
			<label className="field full"><span>充值折算系数</span><input autoFocus inputMode="decimal" value={factorValue} onChange={(event) => setFactorValue(event.target.value)} placeholder="例如 0.4" /></label>
		</div>
		{factorError && <div className="form-error">{factorError}</div>}
		<div className="info-callout"><BadgePercent size={18} /><div><strong>有效倍率 = 上游原始倍率 × 折算系数</strong><span>原始倍率始终保留用于审计；预览、收益评估和自动跟价只使用有效倍率。倍率 3、系数 0.4 时，有效倍率为 1.2。</span></div></div>
	  </Modal>

		<Modal open={!!evidenceSupplier} onClose={() => !evidenceLoading && setEvidenceSupplier(null)} title={`${evidenceSupplier?.name ?? ''} · 采集证据`} description="可信目录、最近尝试和权限验证分别记录；一次网络或授权失败不会清空上次成功数据。" footer={<Button variant="secondary" onClick={() => setEvidenceSupplier(null)} disabled={evidenceLoading}>关闭</Button>}>
			{evidenceSupplier && <div className="evidence-summary-grid">
				<div><Clock3 size={17} /><span>最近尝试</span><strong>{formatTimestamp(evidenceSupplier.lastAttemptAt)}</strong></div>
				<div><ShieldCheck size={17} /><span>最近可信成功</span><strong>{formatTimestamp(evidenceSupplier.lastSuccessAt)}</strong></div>
				<div><Activity size={17} /><span>成功 / 失败耗时</span><strong>{evidenceSupplier.latencyMs !== null ? `${evidenceSupplier.latencyMs} ms` : '待采样'} / {evidenceSupplier.lastFailureDurationMs !== null ? `${evidenceSupplier.lastFailureDurationMs} ms` : '无失败样本'}</strong></div>
			</div>}
			{evidenceLoading ? <div className="inline-loading"><RefreshCw size={18} className="spin" />读取服务端证据…</div> : evidenceError ? <div className="form-error">{evidenceError}</div> : <div className="evidence-workspace">
				<section className="evidence-capabilities"><header><strong>授权能力</strong><span>捕获凭据后，只有实际访问成功的能力才显示为已验证。</span></header>{capabilities.length === 0 ? <p className="evidence-empty">尚无授权能力样本；公开站点也可以正常采集公开目录。</p> : <div>{capabilities.map((item) => <article key={`${item.capability}:${item.endpoint_type || 'unknown'}`}><StatusBadge status={item.status === 'verified' ? 'healthy' : item.status === 'unknown' ? 'unknown' : 'error'} label={item.status === 'verified' ? '已验证' : item.status === 'unknown' ? '待验证' : '当前不可用'} /><strong>{capabilityName(item.capability)}</strong><span>{item.endpoint_type || '未记录端点'} · {item.http_status ? `HTTP ${item.http_status}` : '无 HTTP 状态'} · {item.duration_ms ?? 0} ms</span>{item.error_message && <small>{item.error_message}</small>}</article>)}</div>}</section>
				<section className="evidence-attempts"><header><strong>最近采集</strong><span>保留最近 20 次尝试及请求级诊断。</span></header>{attempts.length === 0 ? <p className="evidence-empty">尚无新版本采集记录，执行一次采集后即可查看。</p> : <div>{attempts.map((attempt) => <details key={attempt.id} className={`evidence-attempt attempt-${attempt.status}`}><summary><StatusBadge status={attempt.status === 'success' ? 'healthy' : attempt.status === 'partial_success' ? 'warning' : 'error'} label={attempt.status === 'success' ? '成功' : attempt.status === 'partial_success' ? '部分成功' : attempt.status === 'cancelled' ? '已取消' : '失败'} /><strong>{formatTimestamp(attempt.finished_at)}</strong><span>{attempt.duration_ms} ms · {attempt.requests.length} 个请求</span>{attempt.error_class && <code>{attempt.error_class}</code>}</summary><div className="request-evidence-list">{attempt.error_message && <p className="request-error">{attempt.error_message}</p>}{attempt.requests.map((request) => <article key={request.id}><div><strong>{request.endpoint_type || '请求'}</strong><span>{request.method} · {request.http_status ? `HTTP ${request.http_status}` : request.status} · {request.duration_ms} ms</span></div><code>{request.source_url || '端点地址未记录'}</code><small>{capabilityName(request.auth_capability || 'unknown')}{request.error_class ? ` · ${request.error_class}` : ''}{request.error_message ? ` · ${request.error_message}` : ''}</small></article>)}</div></details>)}</div>}</section>
			</div>}
		</Modal>
    </>
  );
}
