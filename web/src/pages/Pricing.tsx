import { useEffect, useMemo, useState } from 'react';
import { AlertTriangle, ArrowRight, Calculator, CircleDollarSign, ClipboardCheck, LoaderCircle, Pause, Pencil, Play, Plus, RefreshCw, Search, ShieldCheck, SlidersHorizontal, Trash2, Workflow, XCircle } from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { AutomationConfirmationConfig, AutomationSafetyConfig, CatalogGroup, CreatePricingPolicyInput, PolicyMode, PolicySimulationResult, PricingPolicy, PricingPolicyBulkMutationInput, PricingPolicyBulkPreview, PricingPolicyBulkResult, PricingPolicyBulkStrategy, PricingPolicyDeletionImpact, RawPricingPolicy, RawSupplier, TargetSite } from '../api/types';
import { Button, EmptyState, formatDateTime, IconButton, Modal, modeLabel, PageHeader, Section, Toggle } from '../components/ui';
import { preferredEnabledTarget } from '../data/targets';

type RuleForm = {
  name: string;
  supplierId: string;
  sourceGroup: string;
  sourceBasis: 'group_ratio';
  targetId: string;
  targetGroup: string;
  customTargetGroup: string;
  mode: PolicyMode;
  parameter: string;
  minimum: string;
  maximum: string;
  rounding: string;
  riskBuffer: string;
  subsidyBudget: string;
  allowNegative: boolean;
  executionMode: 'approval' | 'auto';
  coverageMode: 'strict' | 'threshold' | 'owner_override';
  minimumCoveragePercent: string;
};

const initialForm: RuleForm = {
  name: '', supplierId: '', sourceGroup: '', targetId: '', targetGroup: '', customTargetGroup: '',
	sourceBasis: 'group_ratio',
  mode: 'multiply', parameter: '1.2', minimum: '0', maximum: '1', rounding: '0.001',
  riskBuffer: '0', subsidyBudget: '0', allowNegative: false, executionMode: 'approval',
  coverageMode: 'threshold', minimumCoveragePercent: '80',
};

function isPercentageMode(mode: PolicyMode) {
	return mode === 'target_margin' || mode === 'subsidy_rate' || mode === 'markup';
}

function formatPolicyParameter(mode: PolicyMode, value: number) {
	if (!Number.isFinite(value)) return '无效参数';
	const displayed = isPercentageMode(mode) ? value * 100 : value;
	return `${displayed.toLocaleString('zh-CN', { maximumFractionDigits: 6 })}${isPercentageMode(mode) ? '%' : ''}`;
}

function percentSetting(raw: string) {
	if (raw.trim() === '') return '';
	const value = Number(raw);
	return Number.isFinite(value) ? value * 100 : '';
}

function decimalPercentInput(raw: string) {
	if (raw.trim() === '') return '';
	const value = Number(raw);
	return Number.isFinite(value) ? String(value / 100) : raw;
}

type BulkPolicyAction = {
	title: string;
	description: string;
	mutation: Pick<PricingPolicyBulkMutationInput, 'execution_mode' | 'enabled'>;
};

export function PricingPage({ policies, automationEnabled, backgroundWorkersEnabled, remoteWritesEnabled, togglingPolicyIds, onAutomationChange, onToggle, onSavePolicy, onReload, onOpenMirror }: { policies: PricingPolicy[]; automationEnabled: boolean; backgroundWorkersEnabled: boolean; remoteWritesEnabled: boolean; togglingPolicyIds: Set<string>; onAutomationChange: (enabled: boolean) => Promise<void>; onToggle: (id: string, enabled: boolean) => void; onSavePolicy: (input: CreatePricingPolicyInput, policyId?: string, syncTargetWriteMode?: boolean) => Promise<{ warning?: string }>; onReload: () => Promise<void>; onOpenMirror: () => void }) {
  const [createOpen, setCreateOpen] = useState(false);
  const [simulateOpen, setSimulateOpen] = useState(false);
  const [selected, setSelected] = useState<PricingPolicy | null>(null);
	const [editingId, setEditingId] = useState<string | null>(null);
	const [simulation, setSimulation] = useState<PolicySimulationResult | null>(null);
	const [simulationLoading, setSimulationLoading] = useState(false);
	const [simulationError, setSimulationError] = useState('');
  const [suppliers, setSuppliers] = useState<RawSupplier[]>([]);
  const [targets, setTargets] = useState<TargetSite[]>([]);
  const [sourceGroups, setSourceGroups] = useState<CatalogGroup[]>([]);
  const [targetGroups, setTargetGroups] = useState<string[]>([]);
  const [form, setForm] = useState<RuleForm>(initialForm);
  const [optionsLoading, setOptionsLoading] = useState(false);
  const [groupLoading, setGroupLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [formError, setFormError] = useState('');
	const [automationSaving, setAutomationSaving] = useState(false);
	const [automationError, setAutomationError] = useState('');
	const [confirmation, setConfirmation] = useState<AutomationConfirmationConfig | null>(null);
	const [safety, setSafety] = useState<AutomationSafetyConfig | null>(null);
	const [automationSettingsSaving, setAutomationSettingsSaving] = useState(false);
	const [automationSettingsError, setAutomationSettingsError] = useState('');
	const [syncTargetWriteMode, setSyncTargetWriteMode] = useState(true);
	const [policyQuery, setPolicyQuery] = useState('');
	const [policyModeFilter, setPolicyModeFilter] = useState<'all' | 'approval' | 'auto'>('all');
	const [policyStateFilter, setPolicyStateFilter] = useState<'all' | 'enabled' | 'paused'>('all');
	const [selectedPolicyIds, setSelectedPolicyIds] = useState<Set<string>>(new Set());
	const [bulkOpen, setBulkOpen] = useState(false);
	const [bulkAction, setBulkAction] = useState<BulkPolicyAction | null>(null);
	const [bulkInput, setBulkInput] = useState<PricingPolicyBulkMutationInput | null>(null);
	const [bulkPreview, setBulkPreview] = useState<PricingPolicyBulkPreview | null>(null);
	const [bulkResult, setBulkResult] = useState<PricingPolicyBulkResult | null>(null);
	const [bulkStrategy, setBulkStrategy] = useState<PricingPolicyBulkStrategy>('atomic');
	const [bulkLoading, setBulkLoading] = useState(false);
	const [bulkError, setBulkError] = useState('');
	const [deletePolicy, setDeletePolicy] = useState<PricingPolicy | null>(null);
	const [deleteImpact, setDeleteImpact] = useState<PricingPolicyDeletionImpact | null>(null);
	const [deleteConfirmName, setDeleteConfirmName] = useState('');
	const [deleteLoading, setDeleteLoading] = useState(false);
	const [deleteError, setDeleteError] = useState('');
	useEffect(() => {
		void Promise.all([api.automationConfirmation(), api.automationSafety()])
			.then(([confirmationResult, safetyResult]) => { setConfirmation(confirmationResult.data); setSafety(safetyResult.data); })
			.catch((error) => setAutomationSettingsError(`自动化设置读取失败：${getErrorMessage(error)}`));
	}, []);
  const active = policies.filter((policy) => policy.enabled).length;
	const activeAutomatic = policies.filter((policy) => policy.enabled && policy.executionMode === 'auto').length;
	const subsidyPolicies = policies.filter((policy) => policy.allowNegativeMargin).length;
	const selectedTarget = targets.find((item) => String(item.id) === form.targetId);
	const filteredPolicies = useMemo(() => {
		const query = policyQuery.trim().toLowerCase();
		return policies.filter((policy) => {
			const matchesQuery = !query || `${policy.name} ${policy.supplier} ${policy.upstreamGroup} ${policy.targetGroup}`.toLowerCase().includes(query);
			const matchesMode = policyModeFilter === 'all' || policy.executionMode === policyModeFilter;
			const matchesState = policyStateFilter === 'all' || (policyStateFilter === 'enabled' ? policy.enabled : !policy.enabled);
			return matchesQuery && matchesMode && matchesState;
		});
	}, [policies, policyModeFilter, policyQuery, policyStateFilter]);
	const selectedIDs = useMemo(() => policies.filter((policy) => selectedPolicyIds.has(policy.id)).map((policy) => Number(policy.id)).filter((id) => Number.isInteger(id) && id > 0), [policies, selectedPolicyIds]);
	const selectedVisibleCount = filteredPolicies.filter((policy) => selectedPolicyIds.has(policy.id)).length;
	const hiddenSelectedCount = selectedIDs.length - selectedVisibleCount;
	const allFilteredSelected = filteredPolicies.length > 0 && selectedVisibleCount === filteredPolicies.length;
	const someFilteredSelected = selectedVisibleCount > 0 && !allFilteredSelected;

  const loadSourceGroups = async (supplierId: string) => {
    if (!supplierId) return setSourceGroups([]);
    setGroupLoading(true);
    try {
      const result = await api.supplierSnapshot(supplierId);
      setSourceGroups(result.data.groups);
      const preferred = result.data.groups.find((group) => group.ratio?.trim()) ?? result.data.groups[0];
      setForm((current) => ({ ...current, sourceGroup: result.data.groups.some((group) => group.external_id === current.sourceGroup) ? current.sourceGroup : preferred?.external_id ?? '' }));
    } catch (error) {
      setFormError(getErrorMessage(error));
      setSourceGroups([]);
    } finally {
      setGroupLoading(false);
    }
  };

  const loadTargetGroups = async (targetId: string) => {
    if (!targetId) return setTargetGroups([]);
    setGroupLoading(true);
    try {
      const result = await api.targetSnapshot(targetId);
      const groups = Object.keys(result.data.group_ratio).sort((a, b) => a.localeCompare(b));
      setTargetGroups(groups);
      setForm((current) => ({ ...current, targetGroup: groups.includes(current.targetGroup) ? current.targetGroup : groups[0] ?? '__custom' }));
    } catch (error) {
      setFormError(getErrorMessage(error));
      setTargetGroups([]);
    } finally {
      setGroupLoading(false);
    }
  };

  const formFromPolicy = (raw: RawPricingPolicy, availableTargetGroups: string[]): RuleForm => {
		const percentage = isPercentageMode(raw.rule_type);
		const targetExists = availableTargetGroups.includes(raw.target_group);
		return {
			name: raw.name, supplierId: String(raw.supplier_id), sourceGroup: raw.source_group,
			sourceBasis: 'group_ratio',
			targetId: String(raw.target_site_id), targetGroup: targetExists ? raw.target_group : '__custom',
			customTargetGroup: targetExists ? '' : raw.target_group, mode: raw.rule_type,
			parameter: percentage ? String(Number(raw.value) * 100) : raw.value,
			minimum: raw.minimum_ratio, maximum: raw.maximum_ratio, rounding: raw.rounding_step,
			riskBuffer: raw.risk_buffer_rate, subsidyBudget: raw.subsidy_budget_daily,
			allowNegative: raw.allow_negative_profit, executionMode: raw.execution_mode,
			coverageMode: raw.coverage_mode || 'threshold',
			minimumCoveragePercent: String(Number(raw.minimum_coverage_ratio || '0.8') * 100),
		};
	};

  const openRuleBuilder = async (policy?: PricingPolicy) => {
    setCreateOpen(true);
    setOptionsLoading(true);
    setSaved(false);
    setFormError('');
		setEditingId(policy?.id ?? null);
		setSyncTargetWriteMode(true);
    setForm(initialForm);
    try {
      const [supplierResult, targetResult, policyResult] = await Promise.all([api.listSuppliers(), api.listTargets(), api.listPolicies()]);
		const raw = policy ? policyResult.data.find((item) => String(item.id) === policy.id) : undefined;
		if (policy && !raw) throw new Error('没有找到这条规则的完整配置，请刷新后重试。');
		const availableSuppliers = supplierResult.data.filter((item) => item.enabled || item.id === raw?.supplier_id);
		const availableTargets = targetResult.data.filter((item) => item.enabled || item.id === raw?.target_site_id);
		setSuppliers(availableSuppliers);
		setTargets(availableTargets);
		const supplierId = String(raw?.supplier_id ?? availableSuppliers[0]?.id ?? '');
		const targetId = String(raw?.target_site_id ?? preferredEnabledTarget(availableTargets)?.id ?? '');
		if (!supplierId || !targetId) throw new Error('请先连接至少一个上游站点和一个本站 NewAPI 目标。');
		const [sourceResult, targetSnapshot] = await Promise.all([api.supplierSnapshot(supplierId), api.targetSnapshot(targetId)]);
		const groups = sourceResult.data.groups;
		const targetGroupNames = Object.keys(targetSnapshot.data.group_ratio).sort((a, b) => a.localeCompare(b));
		setSourceGroups(groups);
		setTargetGroups(targetGroupNames);
		if (raw) {
			setForm(formFromPolicy(raw, targetGroupNames));
		} else {
			const preferred = groups.find((group) => group.ratio?.trim()) ?? groups[0];
			setForm({ ...initialForm, supplierId, sourceGroup: preferred?.external_id ?? '', targetId,
				targetGroup: targetGroupNames[0] ?? '__custom', name: availableSuppliers[0] ? `${availableSuppliers[0].name} 价格跟随` : '' });
		}
    } catch (error) {
      setFormError(getErrorMessage(error));
    } finally {
      setOptionsLoading(false);
    }
  };

  const changeSupplier = async (supplierId: string) => {
    const supplier = suppliers.find((item) => String(item.id) === supplierId);
    setForm((current) => ({ ...current, supplierId, sourceGroup: '', name: current.name || `${supplier?.name ?? '上游'} 价格跟随` }));
    setFormError('');
    await loadSourceGroups(supplierId);
  };

  const changeTarget = async (targetId: string) => {
    setForm((current) => ({ ...current, targetId, targetGroup: '' }));
    setFormError('');
    await loadTargetGroups(targetId);
  };

  const submitRule = async () => {
    const targetGroup = form.targetGroup === '__custom' ? form.customTargetGroup.trim() : form.targetGroup;
    if (!form.name.trim() || !form.supplierId || !form.sourceGroup || !form.targetId || !targetGroup || !form.parameter) {
      setFormError('请完整选择上游、来源分组、目标站点和目标分组。');
      return;
    }
	const coveragePercent = Number(form.minimumCoveragePercent);
	if (form.coverageMode === 'threshold' && (!Number.isFinite(coveragePercent) || coveragePercent < 0 || coveragePercent > 100)) {
		setFormError('最低价格维度覆盖率必须在 0% 到 100% 之间。');
		return;
	}
    setSaving(true);
    setFormError('');
	const percentageMode = isPercentageMode(form.mode);
    const value = percentageMode ? String(Number(form.parameter) / 100) : form.parameter;
    try {
      const result = await onSavePolicy({
        name: form.name.trim(),
        supplier_id: Number(form.supplierId),
        source_group: form.sourceGroup,
        source_model_pattern: '*',
		source_basis: 'group_ratio',
        target_site_id: Number(form.targetId),
        target_group: targetGroup,
        rule_type: form.mode,
        value,
        profit_basis: 'contribution',
        revenue_cash_factor: '1',
        cost_cash_factor: '1',
        risk_buffer_rate: form.riskBuffer || '0',
        allow_negative_profit: form.allowNegative,
        subsidy_budget_daily: form.subsidyBudget || '0',
        minimum_ratio: form.minimum || '0',
        maximum_ratio: form.maximum || '0',
        rounding_step: form.rounding || '0.001',
        only_follow_increase: false,
        only_follow_decrease: false,
        enabled: true,
        execution_mode: form.executionMode,
		coverage_mode: form.coverageMode,
		minimum_coverage_ratio: String((form.coverageMode === 'threshold' ? coveragePercent : Number(form.minimumCoveragePercent || '80')) / 100),
		}, editingId ?? undefined, form.executionMode === 'auto' && selectedTarget?.write_mode !== 'auto' && syncTargetWriteMode);
      if (result.warning) {
        setSaved(true);
        setFormError(result.warning);
      } else {
        setCreateOpen(false);
      }
    } catch (error) {
      setFormError(getErrorMessage(error));
    } finally {
      setSaving(false);
    }
  };

  const openSimulation = async (policy: PricingPolicy) => {
    setSelected(policy);
    setSimulateOpen(true);
		setSimulation(null);
		setSimulationError('');
		setSimulationLoading(true);
		try {
			const result = await api.simulatePolicy(policy.id);
			setSimulation(result.data);
		} catch (error) {
			setSimulationError(getErrorMessage(error));
		} finally {
			setSimulationLoading(false);
		}
  };

	const togglePolicySelection = (id: string, checked: boolean) => {
		setSelectedPolicyIds((current) => {
			const next = new Set(current);
			if (checked) next.add(id); else next.delete(id);
			return next;
		});
	};

	const toggleFilteredSelection = (checked: boolean) => {
		setSelectedPolicyIds((current) => {
			const next = new Set(current);
			for (const policy of filteredPolicies) {
				if (checked) next.add(policy.id); else next.delete(policy.id);
			}
			return next;
		});
	};

	const loadBulkPreview = async (input: PricingPolicyBulkMutationInput) => {
		setBulkLoading(true);
		setBulkError('');
		setBulkPreview(null);
		try {
			const response = await api.previewPolicyBulkMutation({ ...input, strategy: input.strategy ?? bulkStrategy });
			setBulkPreview(response.data);
		} catch (error) {
			setBulkError(getErrorMessage(error));
		} finally {
			setBulkLoading(false);
		}
	};

	const openBulkAction = async (action: BulkPolicyAction) => {
		if (selectedIDs.length === 0) return;
		const input: PricingPolicyBulkMutationInput = { policy_ids: selectedIDs, ...action.mutation, strategy: 'atomic' };
		setBulkAction(action);
		setBulkInput(input);
		setBulkStrategy('atomic');
		setBulkResult(null);
		setBulkOpen(true);
		await loadBulkPreview(input);
	};

	const applyBulkAction = async () => {
		if (!bulkInput || !bulkPreview) return;
		setBulkLoading(true);
		setBulkError('');
		try {
			const response = await api.applyPolicyBulkMutation({
				...bulkInput, strategy: bulkStrategy, preview_hash: bulkPreview.preview_hash,
			});
			setBulkResult(response.data);
			setSelectedPolicyIds(new Set());
			await onReload();
		} catch (error) {
			setBulkError(getErrorMessage(error));
		} finally {
			setBulkLoading(false);
		}
	};

	const bulkCanApply = Boolean(bulkPreview && bulkPreview.summary.changed > 0
		&& (bulkStrategy === 'best_effort' || bulkPreview.summary.blocked === 0));

	const toggleAutomation = async (enabled: boolean) => {
		if (!remoteWritesEnabled) {
			setAutomationError('本地验收候选禁止远程写入；请在正式可写实例中启用无人值守改价。');
			return;
		}
		setAutomationSaving(true);
		setAutomationError('');
		try {
			await onAutomationChange(enabled);
		} catch (error) {
			setAutomationError(getErrorMessage(error));
		} finally {
			setAutomationSaving(false);
		}
	};

	const saveAutomationSettings = async () => {
		if (!confirmation || !safety) return;
		setAutomationSettingsSaving(true);
		setAutomationSettingsError('');
		try {
			const [confirmationResult, safetyResult] = await Promise.all([
				api.updateAutomationConfirmation(confirmation),
				api.updateAutomationSafety(safety),
			]);
			setConfirmation(confirmationResult.data);
			setSafety(safetyResult.data);
		} catch (error) {
			setAutomationSettingsError(`自动化设置保存失败：${getErrorMessage(error)}`);
		} finally {
			setAutomationSettingsSaving(false);
		}
	};

	const openDeletePolicy = async (policy: PricingPolicy) => {
		setDeletePolicy(policy);
		setDeleteImpact(null);
		setDeleteConfirmName('');
		setDeleteError('');
		setDeleteLoading(true);
		try {
			const result = await api.pricingPolicyDeletionImpact(policy.id);
			setDeleteImpact(result.data);
		} catch (error) {
			setDeleteError(getErrorMessage(error));
		} finally {
			setDeleteLoading(false);
		}
	};

	const confirmDeletePolicy = async () => {
		if (!deletePolicy || !deleteImpact) return;
		setDeleteLoading(true);
		setDeleteError('');
		try {
			await api.deletePricingPolicy(deletePolicy.id, { confirm_name: deleteConfirmName, impact_hash: deleteImpact.impact_hash });
			setSelectedPolicyIds((current) => { const next = new Set(current); next.delete(deletePolicy.id); return next; });
			setDeletePolicy(null);
			setDeleteImpact(null);
			setDeleteConfirmName('');
			await onReload();
		} catch (error) {
			setDeleteError(getErrorMessage(error));
			try {
				const refreshed = await api.pricingPolicyDeletionImpact(deletePolicy.id);
				setDeleteImpact(refreshed.data);
			} catch {
				// Preserve the primary failure when the rule disappeared concurrently.
			}
		} finally {
			setDeleteLoading(false);
		}
	};

  return (
    <>
	      <PageHeader eyebrow="定价自动化" title="价格映射与利润规则" description="从上游真实成本推导本站售价。规则可创建、编辑、暂停、模拟和恢复，公益负利润也能被准确计入。" actions={<><Button variant="secondary" onClick={onOpenMirror}><Workflow size={17} />分组镜像</Button><Button onClick={() => void openRuleBuilder()}><Plus size={17} />新建规则</Button></>} />
      <div className={`policy-banner ${automationEnabled && remoteWritesEnabled ? '' : 'is-disabled'}`}>
        <div><span className="banner-icon"><Calculator size={21} /></span><div><strong>{!remoteWritesEnabled ? '本地验收候选禁写' : automationEnabled ? `${activeAutomatic} 条自动规则已启用` : '无人值守改价总开关已关闭'}</strong><p>{!remoteWritesEnabled ? '当前实例可查看规则、模拟和预览，但服务端硬门禁禁止改价；总开关已在页面内同步锁定。' : automationEnabled ? `${active} 条规则处于运行状态；每次写入仍会重新核对证据、权限和读回。` : `${active} 条规则仍可模拟和人工审批，但后台不会自动修改本站价格。`}</p></div></div>
        <div className="policy-banner-controls"><div className="policy-banner-stats"><span>待确认<strong>{policies.filter((item) => item.suggestedRate !== null && item.currentRate !== null && Math.abs(item.suggestedRate - item.currentRate) > .0001).length}</strong></span><span>公益规则<strong>{subsidyPolicies}</strong></span></div><label className="automation-master-switch"><span><strong>{!remoteWritesEnabled ? '本地验收候选禁写' : automationEnabled ? '无人值守已开启' : '无人值守已暂停'}</strong><small>{!remoteWritesEnabled ? '服务端远程写门禁已关闭' : backgroundWorkersEnabled ? '后台调度运行中' : '后台调度未启动'}</small></span>{automationSaving ? <LoaderCircle size={20} className="spin" /> : <Toggle checked={automationEnabled} onChange={(enabled) => void toggleAutomation(enabled)} label={remoteWritesEnabled ? '无人值守自动改价总开关' : '本地验收候选禁写'} disabled={!remoteWritesEnabled || (!backgroundWorkersEnabled && !automationEnabled)} />}</label></div>
      </div>
	  {automationError && <div className="form-error automation-control-error">总开关更新失败：{automationError}</div>}
	  {confirmation && safety && <details className="automation-settings-panel">
		<summary><span className="automation-settings-icon"><ShieldCheck size={18} /></span><span><strong>无人值守设置</strong><small>目标站读取会自动合并，避免触发访问频率限制。</small></span><span className="automation-settings-summary">共享缓存 {safety.target_read_cache_milliseconds / 60000} 分钟</span></summary>
		<div className="automation-settings-grid">
			<section><div className="automation-settings-heading"><RefreshCw size={16} /><span><strong>价差即时确认</strong><small>只有真实价差且唯一缺少连续证据时才补采。</small></span></div><label className="check-row"><input type="checkbox" checked={confirmation.enabled} disabled={automationSettingsSaving} onChange={(event) => setConfirmation({ ...confirmation, enabled: event.target.checked })} /><span>启用定向复采</span></label><div className="automation-settings-fields"><label className="field"><span>复采间隔</span><input type="number" min={1} max={60} value={confirmation.interval_seconds} disabled={automationSettingsSaving} onChange={(event) => setConfirmation({ ...confirmation, interval_seconds: Number(event.target.value) })} /><small>1–60 秒</small></label><label className="field"><span>最多补采</span><input type="number" min={1} max={4} value={confirmation.max_attempts} disabled={automationSettingsSaving} onChange={(event) => setConfirmation({ ...confirmation, max_attempts: Number(event.target.value) })} /><small>1–4 次</small></label></div></section>
			<section><div className="automation-settings-heading"><SlidersHorizontal size={16} /><span><strong>资金安全阈值</strong><small>超过阈值仍保留建议值，但不会无人值守写入。</small></span></div><div className="automation-settings-fields safety-fields"><label className="field"><span>目标读取缓存</span><input type="number" min={1} max={60} step={1} value={safety.target_read_cache_milliseconds / 60000} disabled={automationSettingsSaving} onChange={(event) => setSafety({ ...safety, target_read_cache_milliseconds: Math.round(Number(event.target.value) * 60000) })} /><small>分钟，建议 30</small></label><label className="field"><span>等价成本上限</span><input type="number" min={0} max={1000} step={0.1} value={safety.equivalent_cost_multiple_limit} disabled={automationSettingsSaving} onChange={(event) => setSafety({ ...safety, equivalent_cost_multiple_limit: event.target.value })} /><small>相对有效倍率，0 为关闭</small></label><label className="field"><span>最少可比维度</span><input type="number" min={0} max={1000} value={safety.minimum_comparable_dimensions} disabled={automationSettingsSaving} onChange={(event) => setSafety({ ...safety, minimum_comparable_dimensions: Number(event.target.value) })} /><small>0 为关闭</small></label><label className="field"><span>最大单次涨幅</span><input type="number" min={0} max={100000} step={1} value={percentSetting(safety.maximum_automatic_increase_rate)} disabled={automationSettingsSaving} onChange={(event) => setSafety({ ...safety, maximum_automatic_increase_rate: decimalPercentInput(event.target.value) })} /><small>%，0 为关闭</small></label><label className="field"><span>最大单次降幅</span><input type="number" min={0} max={100000} step={1} value={percentSetting(safety.maximum_automatic_decrease_rate)} disabled={automationSettingsSaving} onChange={(event) => setSafety({ ...safety, maximum_automatic_decrease_rate: decimalPercentInput(event.target.value) })} /><small>%，0 为关闭</small></label></div></section>
		</div>
		<div className="automation-settings-footer"><span>默认：30 分钟共享读取、4 倍异常门槛、至少 2 个可比维度、最大上涨 100% / 下降 75%。</span><Button onClick={() => void saveAutomationSettings()} disabled={automationSettingsSaving}>{automationSettingsSaving ? <LoaderCircle size={16} className="spin" /> : <ShieldCheck size={16} />}{automationSettingsSaving ? '保存中' : '保存并立即生效'}</Button></div>
	  </details>}
	  {automationSettingsError && <div className="form-error automation-control-error">{automationSettingsError}</div>}
      <Section title="映射规则" description="一个目标定价域只允许一条生效规则，避免重复改价">
		<div className="policy-filter-toolbar">
			<div className="toolbar-search"><Search size={16} /><input value={policyQuery} onChange={(event) => setPolicyQuery(event.target.value)} placeholder="搜索规则、上游或分组" /></div>
			<div className="segmented" aria-label="执行方式筛选">
				<button type="button" className={policyModeFilter === 'all' ? 'active' : ''} onClick={() => setPolicyModeFilter('all')}>全部方式</button>
				<button type="button" className={policyModeFilter === 'approval' ? 'active' : ''} onClick={() => setPolicyModeFilter('approval')}>人工确认</button>
				<button type="button" className={policyModeFilter === 'auto' ? 'active' : ''} onClick={() => setPolicyModeFilter('auto')}>自动应用</button>
			</div>
			<label className="policy-state-filter"><span>运行状态</span><select value={policyStateFilter} onChange={(event) => setPolicyStateFilter(event.target.value as typeof policyStateFilter)}><option value="all">全部</option><option value="enabled">运行中</option><option value="paused">已暂停</option></select></label>
		</div>
		<div className={`policy-bulk-toolbar ${selectedIDs.length > 0 ? 'has-selection' : ''}`}>
			<label className="policy-select-all">
				<input type="checkbox" checked={allFilteredSelected} ref={(node) => { if (node) node.indeterminate = someFilteredSelected; }} onChange={(event) => toggleFilteredSelection(event.target.checked)} disabled={filteredPolicies.length === 0} />
				<span>{allFilteredSelected ? '取消当前筛选' : '全选当前筛选'}</span>
			</label>
			<div className="policy-selection-count"><strong>{selectedIDs.length}</strong><span>条已选</span>{hiddenSelectedCount > 0 && <em>{hiddenSelectedCount} 条在筛选外</em>}{selectedIDs.length > 0 && <button type="button" onClick={() => setSelectedPolicyIds(new Set())}>清空</button>}</div>
			<div className="policy-bulk-actions">
				<Button variant="secondary" disabled={selectedIDs.length === 0 || bulkLoading} onClick={() => void openBulkAction({ title: '启用并切换为自动应用', description: '一次启用所选规则并切换为自动模式；目标站必须已经允许受控自动写入，全局自动化总开关关闭时仍不会远端改价。', mutation: { execution_mode: 'auto', enabled: true } })}><Workflow size={15} />自动并启用</Button>
				<Button variant="secondary" disabled={selectedIDs.length === 0 || bulkLoading} onClick={() => void openBulkAction({ title: '切换为人工确认', description: '后续价格变化只生成建议，不再由规则自动写入。', mutation: { execution_mode: 'approval' } })}><ClipboardCheck size={15} />设为审批</Button>
				<Button variant="secondary" disabled={selectedIDs.length === 0 || bulkLoading} onClick={() => void openBulkAction({ title: '批量启用规则', description: '恢复规则计算；自动规则仍会检查目标站写权限。', mutation: { enabled: true } })}><Play size={15} />启用</Button>
				<Button variant="ghost" disabled={selectedIDs.length === 0 || bulkLoading} onClick={() => void openBulkAction({ title: '批量暂停规则', description: '暂停规则计算，不改变公式、映射和历史审计。', mutation: { enabled: false } })}><Pause size={15} />暂停</Button>
			</div>
		</div>
		{filteredPolicies.length === 0 ? <EmptyState icon={<Search size={22} />} title="当前筛选下没有规则" description="调整搜索词、执行方式或运行状态后再试。" /> : <div className="policy-list">
          {filteredPolicies.map((policy) => {
            const changed = policy.suggestedRate !== null && policy.currentRate !== null && Math.abs(policy.suggestedRate - policy.currentRate) > .0001;
			const noPriceChange = !changed && policy.suggestedRate !== null && policy.currentRate !== null;
			const blocked = changed && (!policy.enabled || (policy.executionMode === 'auto' && (!automationEnabled || policy.targetWriteMode !== 'auto' || policy.proposalMode === 'approval' || policy.automationEligible === false || policy.proposalStatus === 'failed')));
			const automationLabel = !policy.enabled ? '规则已暂停' : noPriceChange ? '当前已匹配，无需执行' : policy.executionMode === 'auto' ? blocked ? '自动执行待补条件' : '自动应用已就绪' : '管理员确认后应用';
			const statusDetail = noPriceChange ? (policy.lastRunAt ? `最近核对 ${formatDateTime(policy.lastRunAt)}` : '最新读数与本站一致') : policy.proposalError || policy.automationReason || policy.lastRunAt || '尚未运行';
			const ruleBasis = policy.ruleBasisRate ?? (policy.sourceBasis === 'group_ratio' ? policy.upstreamRate : null);
            return (
			  <article className={`policy-row ${!policy.enabled ? 'is-disabled' : ''} ${selectedPolicyIds.has(policy.id) ? 'is-selected' : ''}`} key={policy.id}>
				<div className="policy-select"><input type="checkbox" checked={selectedPolicyIds.has(policy.id)} onChange={(event) => togglePolicySelection(policy.id, event.target.checked)} aria-label={`选择规则 ${policy.name}`} /></div>
				<div className="policy-toggle"><Toggle checked={policy.enabled} disabled={togglingPolicyIds.has(policy.id)} onChange={(value) => onToggle(policy.id, value)} label={`${policy.enabled ? '暂停' : '启用'}${policy.name}`} /></div>
                <div className="policy-name"><strong>{policy.name}</strong><span>{policy.supplier} / {policy.upstreamGroup} → {policy.targetGroup}</span></div>
                <div className="policy-formula"><span>{modeLabel(policy.mode)}</span><strong>{formatPolicyParameter(policy.mode, policy.parameter)}</strong><small>{policy.coverageMode === 'strict' ? '全维度' : policy.coverageMode === 'owner_override' ? '所有者覆盖' : `覆盖 ${(policy.minimumCoverage * 100).toFixed(0)}%`}</small></div>
                <div className="policy-numbers"><span>{policy.sourceBasis === 'group_ratio' ? '有效分组倍率' : '等价成本基准'}<strong>{ruleBasis ?? '待证据'}</strong></span><ArrowRight size={15} /><span>建议<strong className={changed ? 'warning' : ''}>{policy.suggestedRate ?? '待模拟'}</strong></span><span>当前<strong>{policy.currentRate ?? (policy.currentRateStatus === 'missing_group' ? '分组不存在' : '待读回')}</strong></span></div>
				<div className="policy-margin"><span>规则价差</span><strong className={policy.ruleMargin === null ? '' : policy.ruleMargin < 0 ? 'negative' : policy.ruleMargin < 12 ? 'warning' : 'positive'}>{policy.ruleMargin === null ? '待计算' : `${policy.ruleMargin.toFixed(1)}%`}</strong><small>{policy.sourceBasis === 'group_ratio' ? '按折算后分组倍率' : '按逐模型等价成本'}</small><small className={blocked ? 'warning' : 'positive'}>{automationLabel}</small></div>
				<div className="policy-action"><div className="policy-action-buttons"><Button variant="secondary" onClick={() => void openSimulation(policy)}><SlidersHorizontal size={16} />查看模拟</Button><Button variant="ghost" onClick={() => void openRuleBuilder(policy)}><Pencil size={16} />编辑</Button><IconButton className="icon-button-danger" label={`删除规则 ${policy.name}`} onClick={() => void openDeletePolicy(policy)}><Trash2 size={16} /></IconButton></div><small>{statusDetail}</small></div>
              </article>
            );
          })}
		</div>}
      </Section>

	  <Modal open={bulkOpen} onClose={() => !bulkLoading && setBulkOpen(false)} title={bulkAction?.title ?? '批量处理规则'} description={bulkAction?.description} footer={bulkResult ? <Button onClick={() => setBulkOpen(false)}>完成</Button> : <><Button variant="ghost" onClick={() => setBulkOpen(false)} disabled={bulkLoading}>取消</Button><Button onClick={() => void applyBulkAction()} disabled={bulkLoading || !bulkCanApply}>{bulkLoading && <LoaderCircle size={16} className="spin" />}{bulkLoading ? '正在提交' : bulkStrategy === 'atomic' ? '确认全部提交' : '应用可执行项'}</Button></>}>
		{bulkLoading && !bulkPreview && <div className="inline-loading"><LoaderCircle className="spin" size={20} />正在核对规则状态与目标站写权限…</div>}
		{bulkError && <div className="form-error bulk-policy-error"><XCircle size={16} />{bulkError}<Button variant="ghost" onClick={() => bulkInput && void loadBulkPreview(bulkInput)} disabled={bulkLoading}><RefreshCw size={15} />重新预检</Button></div>}
		{bulkPreview && !bulkResult && <div className="bulk-policy-preview">
			<div className="bulk-impact-summary"><div><span>已选择</span><strong>{bulkPreview.summary.selected}</strong></div><div><span>将修改</span><strong className="positive">{bulkPreview.summary.changed}</strong></div><div><span>无需修改</span><strong>{bulkPreview.summary.unchanged}</strong></div><div><span>被阻断</span><strong className={bulkPreview.summary.blocked > 0 ? 'negative' : ''}>{bulkPreview.summary.blocked}</strong></div></div>
			<div className={`impact-note ${bulkPreview.summary.blocked > 0 ? 'warning-note' : ''}`}>{bulkPreview.summary.blocked > 0 ? <AlertTriangle size={18} /> : <ShieldCheck size={18} />}<div><strong>{bulkPreview.summary.blocked > 0 ? '部分规则未通过预检' : '全部规则已通过预检'}</strong><span>{bulkPreview.summary.blocked > 0 ? '默认原子提交不会修改任何规则。检查下方原因，或明确选择仅应用可执行项。' : '提交只更新规则运行状态，不会在本次操作中访问上游或修改本站价格。'}</span></div></div>
			{bulkPreview.summary.blocked > 0 && <div className="bulk-strategy"><span>提交策略</span><div className="segmented"><button type="button" className={bulkStrategy === 'atomic' ? 'active' : ''} onClick={() => setBulkStrategy('atomic')}>全部成功才提交</button><button type="button" className={bulkStrategy === 'best_effort' ? 'active' : ''} disabled={bulkPreview.summary.changed === 0} onClick={() => setBulkStrategy('best_effort')}>仅应用可执行项</button></div></div>}
			{bulkPreview.targets.length > 0 && <div className="bulk-target-checks"><div className="bulk-detail-heading"><strong>目标站权限</strong><span>{bulkPreview.summary.targets_affected} 个目标站</span></div>{bulkPreview.targets.map((target) => <div className="bulk-target-row" key={target.target_site_id}><span className={target.auto_write_ready ? 'ready' : 'blocked'}>{target.auto_write_ready ? <ShieldCheck size={15} /> : <AlertTriangle size={15} />}</span><div><strong>{target.target_site_name}</strong><small>{target.message} · 关联 {target.policy_count} 条规则</small></div><code>{target.requires_auto_write ? target.write_mode : '无需检查'}</code></div>)}</div>}
			<div className="bulk-policy-items"><div className="bulk-detail-heading"><strong>逐条结果</strong><span>预览哈希 {bulkPreview.preview_hash.slice(0, 10)}</span></div>{bulkPreview.items.map((item) => <div className={`bulk-policy-item status-${item.status}`} key={item.policy_id}><span>{item.status === 'ready' ? '可提交' : item.status === 'unchanged' ? '无需修改' : '已阻断'}</span><div><strong>{item.policy_name || `规则 #${item.policy_id}`}</strong><small>{item.message}{item.target_site_name ? ` · ${item.target_site_name}` : ''}</small></div></div>)}</div>
		</div>}
		{bulkResult && <div className="bulk-policy-result"><div className={`impact-note ${bulkResult.status === 'partial' || bulkResult.status === 'blocked' ? 'warning-note' : ''}`}>{bulkResult.status === 'applied' || bulkResult.status === 'no_change' ? <ShieldCheck size={18} /> : <AlertTriangle size={18} />}<div><strong>{bulkResult.status === 'applied' ? '批量操作已原子提交' : bulkResult.status === 'partial' ? '可执行项已提交' : bulkResult.status === 'no_change' ? '规则无需修改' : '没有规则被修改'}</strong><span>已修改 {bulkResult.summary.changed} 条，无需修改 {bulkResult.summary.unchanged} 条，阻断 {bulkResult.summary.blocked} 条。价格和目标站权限均未在本次操作中改变。</span></div></div><div className="bulk-policy-items">{bulkResult.items.map((item) => <div className={`bulk-policy-item status-${item.status}`} key={item.policy_id}><span>{item.status === 'applied' ? '已修改' : item.status === 'unchanged' ? '无变化' : '未修改'}</span><div><strong>{item.policy_name || `规则 #${item.policy_id}`}</strong><small>{item.message}</small></div></div>)}</div></div>}
	  </Modal>

      <Modal open={!!deletePolicy} onClose={() => !deleteLoading && setDeletePolicy(null)} title={`删除规则 ${deletePolicy?.name ?? ''}`} description="删除后停止这条规则的计算和自动执行；已经写入本站的价格不会被隐式回滚。" footer={<><Button variant="ghost" onClick={() => setDeletePolicy(null)} disabled={deleteLoading}>取消</Button><Button variant="danger" onClick={() => void confirmDeletePolicy()} disabled={deleteLoading || !deleteImpact || deleteConfirmName.trim() !== deleteImpact.policy_name || deleteImpact.active_leases > 0}>{deleteLoading && <LoaderCircle size={16} className="spin" />}{deleteLoading ? '正在核对' : '确认删除规则'}</Button></>}>
		{deleteLoading && !deleteImpact ? <div className="inline-loading"><LoaderCircle size={18} className="spin" />核对提案、学习证据和执行租约…</div> : deleteImpact && <div className="deletion-impact">
			<div className="danger-callout"><Trash2 size={18} /><div><strong>删除规则，不回滚远端价格</strong><span>目标分组 {deleteImpact.target_group} 保持当前倍率；如需改价，请另建规则或使用受控价格操作。</span></div></div>
			<div className="deletion-impact-grid"><div><span>变更提案</span><strong>{deleteImpact.change_proposals}</strong><small>随规则删除</small></div><div><span>决策学习记录</span><strong>{deleteImpact.decision_records}</strong><small>规则专属记录</small></div><div><span>上架历史引用</span><strong>{deleteImpact.listing_references}</strong><small>保留历史并解除规则引用</small></div><div><span>进行中操作</span><strong>{deleteImpact.active_leases}</strong><small>大于 0 时禁止删除</small></div></div>
			{deleteImpact.active_leases > 0 && <div className="form-error">目标站仍有受控写入正在执行，完成后请重新打开删除窗口。</div>}
			<label className="field"><span>输入完整规则名称以确认</span><input autoComplete="off" value={deleteConfirmName} onChange={(event) => setDeleteConfirmName(event.target.value)} placeholder={deleteImpact.policy_name} /><small>必须精确输入：{deleteImpact.policy_name}</small></label>
		</div>}
		{deleteError && <div className="form-error">{deleteError}</div>}
	  </Modal>

      <Modal open={createOpen} onClose={() => !saving && setCreateOpen(false)} title={editingId ? '编辑价格映射规则' : '新建价格映射规则'} description={editingId ? '保存后会读回完整规则，并使用最新目录重新模拟。' : '选项直接来自当前上游快照和本站目标配置。保存后会立即模拟并生成变更建议。'} footer={saved ? <Button onClick={() => setCreateOpen(false)}>关闭并查看规则</Button> : <><Button variant="ghost" onClick={() => setCreateOpen(false)} disabled={saving}>取消</Button><Button onClick={() => void submitRule()} disabled={saving || optionsLoading}>{saving && <LoaderCircle size={16} className="spin" />}{saving ? '正在保存与模拟' : editingId ? '保存修改并重新预览' : '保存并生成预览'}</Button></>}>
        {optionsLoading ? <div className="inline-loading"><LoaderCircle className="spin" size={20} />正在读取上游、目标站点和价格分组…</div> : <>
          <div className="form-grid">
            <label className="field full"><span>规则名称</span><input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /></label>
            <label className="field"><span>上游站点</span><select value={form.supplierId} onChange={(event) => void changeSupplier(event.target.value)}><option value="">请选择</option>{suppliers.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>
            <label className="field"><span>上游分组 {groupLoading && '· 读取中'}</span><select value={form.sourceGroup} onChange={(event) => setForm({ ...form, sourceGroup: event.target.value })} disabled={groupLoading}><option value="">请选择</option>{sourceGroups.map((group) => <option key={group.external_id} value={group.external_id}>{group.name} · {group.ratio || '未标价'} · {group.model_ids.length} 模型</option>)}</select></label>
			<label className="field"><span>跟价来源口径</span><strong>上游真实分组倍率</strong><small>逐模型价格由分组镜像独立同步。</small></label>
            <label className="field"><span>本站目标站点</span><select value={form.targetId} onChange={(event) => void changeTarget(event.target.value)}><option value="">请选择</option>{targets.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>
            <label className="field"><span>本站目标分组</span><select value={form.targetGroup} onChange={(event) => setForm({ ...form, targetGroup: event.target.value })}><option value="">请选择</option>{targetGroups.map((group) => <option key={group} value={group}>{group}</option>)}<option value="__custom">创建新分组…</option></select></label>
            {form.targetGroup === '__custom' && <label className="field full"><span>新分组名称</span><input value={form.customTargetGroup} onChange={(event) => setForm({ ...form, customTargetGroup: event.target.value })} placeholder="例如 summer-gpt" /></label>}
            <label className="field"><span>计算方式</span><select value={form.mode} onChange={(event) => setForm({ ...form, mode: event.target.value as PolicyMode })}><option value="multiply">上游倍率 × 固定倍数</option><option value="add">上游倍率 + 固定值</option><option value="target_margin">保持目标贡献毛利</option><option value="markup">按成本加成</option><option value="subsidy_rate">公益补贴率</option><option value="fixed">固定本站倍率</option></select></label>
            <label className="field"><span>参数{form.mode === 'target_margin' || form.mode === 'subsidy_rate' || form.mode === 'markup' ? '（%）' : ''}</span><input type="number" step="0.01" value={form.parameter} onChange={(event) => setForm({ ...form, parameter: event.target.value })} /></label>
            <label className="field"><span>最低倍率</span><input type="number" step="0.001" value={form.minimum} onChange={(event) => setForm({ ...form, minimum: event.target.value })} /></label>
            <label className="field"><span>最高倍率（0 为不限）</span><input type="number" step="0.001" value={form.maximum} onChange={(event) => setForm({ ...form, maximum: event.target.value })} /></label>
            <label className="field"><span>风险缓冲率</span><input type="number" step="0.01" value={form.riskBuffer} onChange={(event) => setForm({ ...form, riskBuffer: event.target.value })} /></label>
            <label className="field"><span>执行方式</span><select value={form.executionMode} onChange={(event) => setForm({ ...form, executionMode: event.target.value as RuleForm['executionMode'] })}><option value="approval">生成建议，管理员确认</option><option value="auto">自动应用并读回</option></select></label>
            <label className="field"><span>价格证据策略</span><select value={form.coverageMode} onChange={(event) => setForm({ ...form, coverageMode: event.target.value as RuleForm['coverageMode'] })}><option value="threshold">达到覆盖门槛即可（推荐）</option><option value="strict">必须 100% 完整</option><option value="owner_override">所有者明确覆盖</option></select></label>
            {form.coverageMode === 'threshold' && <label className="field"><span>最低价格维度覆盖率（%）</span><input type="number" min="0" max="100" step="1" value={form.minimumCoveragePercent} onChange={(event) => setForm({ ...form, minimumCoveragePercent: event.target.value })} /></label>}
          </div>
		  {form.coverageMode === 'owner_override' && <div className="impact-note warning-note"><AlertTriangle size={18} /><div><strong>你将保留最终决定权</strong><span>只要存在可比成本维度，系统会允许自动执行；未知图片、缓存、音频或按次维度仍会进入告警和审计，倍率上下限、哈希校验与写后读回不会被跳过。</span></div></div>}
		  {form.executionMode === 'auto' && selectedTarget && <div className={`target-write-gate ${selectedTarget.write_mode === 'auto' ? 'ready' : 'needs-sync'}`}>
			{selectedTarget.write_mode === 'auto' ? <ShieldCheck size={19} /> : <AlertTriangle size={19} />}
			<div><strong>{selectedTarget.write_mode === 'auto' ? '本站目标已允许受控自动写入' : '保存时同步开启本站目标自动写入'}</strong><span>{selectedTarget.write_mode === 'auto' ? '规则满足证据门槛时会立即应用，并通过写后读回确认真实生效。' : '这是自动应用所需的第二道权限。开启后只允许通过预览、哈希校验、串行租约和写后读回的受控价格变更。'}</span></div>
			{selectedTarget.write_mode !== 'auto' && <label className="toggle-label"><Toggle checked={syncTargetWriteMode} onChange={setSyncTargetWriteMode} label="同步开启目标自动写入" />同步开启</label>}
		  </div>}
          <label className="check-row"><input type="checkbox" checked={form.allowNegative} onChange={(event) => setForm({ ...form, allowNegative: event.target.checked })} />允许负利润（公益补贴）<small>开启后系统会把补贴准确计入贡献利润。</small></label>
          {form.allowNegative && <label className="field compact-field"><span>每日补贴预算</span><input type="number" step="0.01" value={form.subsidyBudget} onChange={(event) => setForm({ ...form, subsidyBudget: event.target.value })} /></label>}
        </>}
        {formError && <div className="form-error">{formError}</div>}
      </Modal>

      <Modal open={simulateOpen} onClose={() => setSimulateOpen(false)} title={`${selected?.name ?? ''} · 实时模拟结果`} description="模拟会读取当前上游和本站目录；缺失维度会作为覆盖率与警告展示，不再伪装为完整验证。" footer={<Button onClick={() => setSimulateOpen(false)}>关闭</Button>}>
		{simulationLoading && <div className="inline-loading"><LoaderCircle className="spin" size={20} />正在核对当前价格维度…</div>}
		{simulationError && <div className="form-error">{simulationError}</div>}
		{selected && simulation && <div className="simulation">
		  <div className="simulation-flow"><span>{simulation.policy.source_basis === 'group_ratio' ? '规则基准 · 有效分组倍率' : '规则基准 · 等价成本'}<strong>{simulation.calculation.source_ratio}</strong></span><ArrowRight size={20} /><span>规则结果<strong>{simulation.calculation.target_ratio}</strong></span><ArrowRight size={20} /><span>当前读回<strong>{simulation.current_ratio || '目标分组不存在'}</strong></span></div>
		  <div className="simulation-basis-grid"><div><span>上游原始倍率</span><strong>{simulation.source_group.raw_ratio || simulation.source_group.ratio || '未知'}</strong></div><div><span>充值折算系数</span><strong>{simulation.source_group.recharge_value_factor || '1'}</strong></div><div><span>有效分组倍率</span><strong>{simulation.source_group.effective_ratio || simulation.source_group.ratio || '未知'}</strong></div><div><span>目录保守等价成本</span><strong>{simulation.equivalent_cost_ratio || '不可验证'}</strong></div><div><span>真实现金毛利</span><strong>账本尚未覆盖</strong></div></div>
		  {!simulation.impact_complete && <div className="impact-note warning-note"><AlertTriangle size={18} /><div><strong>{simulation.policy.source_basis === 'group_ratio' ? '目录风险证据不完整，不影响分组倍率跟价' : '当前是部分维度预览'}</strong><span>已比较 {simulation.comparable_dimensions}/{simulation.total_dimensions} 个目录维度。{simulation.policy.source_basis === 'group_ratio' ? '建议值只使用有效分组倍率；等价成本仅作为独立风险提示。' : `自动执行仍由“${simulation.policy.coverage_mode === 'strict' ? '必须完整' : simulation.policy.coverage_mode === 'owner_override' ? '所有者覆盖' : `覆盖率门槛 ${Number(simulation.policy.minimum_coverage_ratio) * 100}%`}”策略决定。`}</span></div></div>}
          <div className="impact-note"><CircleDollarSign size={18} /><div><strong>{simulation.calculation.automation_eligible ? '满足当前自动化门禁' : '当前保持人工审批'}</strong><span>{simulation.calculation.automation_reason || '应用变更时仍会重新读取目标站点、校验哈希并完成写后读回。'}</span></div></div>
		  {simulation.unknown_dimensions.length > 0 && <div className="simulation-details"><strong>待补充维度</strong><div>{simulation.unknown_dimensions.map((item) => <code key={item}>{item}</code>)}</div></div>}
		  {simulation.warnings.length > 0 && <div className="simulation-warnings"><strong>证据说明</strong>{simulation.warnings.map((warning, index) => <p key={`${warning}-${index}`}>{warning}</p>)}</div>}
        </div>}
      </Modal>
    </>
  );
}
