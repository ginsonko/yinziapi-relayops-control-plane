import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { CheckCircle2, Eye, EyeOff, Info, KeyRound, LoaderCircle, ShieldCheck, XCircle } from 'lucide-react';
import { api, ApiError, EXPECTED_API_CONTRACT, getErrorMessage, onApiContractMismatch, reportApiContractMismatch, type ApiContractMismatchDetail } from './api/client';
import { mergeBootstrapData, scopeForPage, type BootstrapScope } from './api/bootstrap';
import type { BootstrapData, ChangeProposal, CollectionJob, ConnectorSpecInput, CreateCredentialInput, CreatePricingPolicyInput, CreateSupplierInput, DeleteSupplierInput, UpdateSupplierInput } from './api/types';
import { Shell, type PageId } from './components/Shell';
import { ErrorState, LoadingState } from './components/ui';
import { ConnectorsPage, type ConnectorIntent } from './pages/Connectors';
import { ConfigPortabilityPage } from './pages/ConfigPortability';
import { IntelligencePage } from './pages/Intelligence';
import { BalancesPage, ChangesPage, DiscoveriesPage } from './pages/Operations';
import { PricingPage } from './pages/Pricing';
import { OverviewPage } from './pages/Overview';
import { WallboardPage } from './pages/Wallboard';
import { WatchlistPage } from './pages/Watchlist';
import { SuppliersPage } from './pages/Suppliers';
import { MappingDraftsPage } from './pages/MappingDrafts';
import { MirrorManagementPage } from './pages/MirrorManagement';

const validPages: PageId[] = ['overview', 'wallboard', 'suppliers', 'watchlist', 'pricing', 'mirror', 'mapping-drafts', 'balances', 'discoveries', 'intelligence', 'changes', 'connectors', 'config-portability'];
async function verifyApiContract() {
	const health = await api.health();
	if (health.data.api_contract_version !== EXPECTED_API_CONTRACT) {
		const mismatch = reportApiContractMismatch(health.data.api_contract_version || '旧版/未知版本');
		throw new Error(`候选版本不匹配：前端需要 ${mismatch.expected}，后端返回 ${mismatch.actual}。请重新加载同一次构建的前后端。`);
	}
}

function pageFromHash(): PageId {
  const value = window.location.hash.replace('#/', '') as PageId;
  return validPages.includes(value) ? value : 'overview';
}

type Toast = { id: number; tone: 'success' | 'info' | 'error'; message: string };
type ProposalActionResult = { ok: boolean; message: string };

function AuthGate({ token, message, submitting, onTokenChange, onSubmit }: {
  token: string;
  message: string;
  submitting: boolean;
  onTokenChange: (value: string) => void;
  onSubmit: () => Promise<void>;
}) {
  const [visible, setVisible] = useState(false);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!submitting && token.trim()) void onSubmit();
  };
  return <main className="auth-gate">
    <section className="auth-panel" aria-labelledby="auth-title">
      <div className="auth-brand"><span><ShieldCheck size={23} /></span><div><strong>RelayOps</strong><small>采购与定价控制台</small></div></div>
      <div className="auth-heading"><span className="auth-key"><KeyRound size={21} /></span><div><h1 id="auth-title">验证管理令牌</h1><p>输入本机 RelayOps 管理令牌后，即可读取控制台。令牌仅保留在当前标签页会话中。</p></div></div>
      <form onSubmit={submit} className="auth-form">
        <label htmlFor="relayops-admin-token">管理令牌</label>
        <div className="auth-input-wrap">
          <KeyRound size={17} />
          <input id="relayops-admin-token" autoFocus type={visible ? 'text' : 'password'} autoComplete="off" spellCheck={false} value={token} onChange={(event) => onTokenChange(event.target.value)} placeholder="粘贴 64 位管理令牌" />
          <button type="button" onClick={() => setVisible((current) => !current)} aria-label={visible ? '隐藏令牌' : '显示令牌'} title={visible ? '隐藏令牌' : '显示令牌'}>{visible ? <EyeOff size={17} /> : <Eye size={17} />}</button>
        </div>
        {message && <div className="auth-error" role="alert"><XCircle size={16} /><span>{message}</span></div>}
        <button className="auth-submit" type="submit" disabled={submitting || !token.trim()}>{submitting ? <LoaderCircle size={17} className="spin" /> : <ShieldCheck size={17} />}{submitting ? '正在验证' : '验证并进入控制台'}</button>
      </form>
      <p className="auth-footnote">管理令牌不会写入网址、活动日志或配置导出。</p>
    </section>
  </main>;
}

export function App() {
  const [page, setPage] = useState<PageId>(pageFromHash);
  const [data, setData] = useState<BootstrapData | null>(null);
  const [loading, setLoading] = useState(true);
  const [fatalError, setFatalError] = useState('');
	const [fatalStatus, setFatalStatus] = useState(0);
	const [authSubmitting, setAuthSubmitting] = useState(false);
  const [syncing, setSyncing] = useState(false);
	const [collectionJob, setCollectionJob] = useState<CollectionJob | null>(null);
  const [collectingId, setCollectingId] = useState<string | null>(null);
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [recoveryToken, setRecoveryToken] = useState(api.getAdminToken());
	const [connectorIntent, setConnectorIntent] = useState<ConnectorIntent | null>(null);
	const [contractMismatch, setContractMismatch] = useState<ApiContractMismatchDetail | null>(null);
	const silentRefreshInFlight = useRef(false);
	const initialPageLoad = useRef(true);
	const bootstrapInFlight = useRef<Map<BootstrapScope, Promise<BootstrapData>>>(new Map());
	const [togglingPolicyIds, setTogglingPolicyIds] = useState<Set<string>>(() => new Set());

  const notify = useCallback((message: string, tone: Toast['tone'] = 'success') => {
    const id = Date.now() + Math.random();
    setToasts((current) => [...current, { id, tone, message }]);
    window.setTimeout(() => setToasts((current) => current.filter((toast) => toast.id !== id)), 4200);
  }, []);

	useEffect(() => onApiContractMismatch(setContractMismatch), []);

	const requestBootstrap = useCallback((scope: BootstrapScope) => {
		const active = bootstrapInFlight.current.get(scope);
		if (active) return active;
		let request: Promise<BootstrapData>;
		request = api.bootstrap(scope).then((result) => result.data).finally(() => {
			if (bootstrapInFlight.current.get(scope) === request) bootstrapInFlight.current.delete(scope);
		});
		bootstrapInFlight.current.set(scope, request);
		return request;
	}, []);

  const load = useCallback(async (showLoading = true, scope: BootstrapScope = 'overview') => {
    if (showLoading) {
      setLoading(true);
      setFatalError('');
		setFatalStatus(0);
    }
    try {
	  if (showLoading) await verifyApiContract();
	  const result = await requestBootstrap(scope);
	  setData((current) => mergeBootstrapData(current, result, scope));
	  setContractMismatch(null);
    } catch (error) {
		if (showLoading) {
			setFatalError(getErrorMessage(error));
			setFatalStatus(error instanceof ApiError ? error.status : 0);
		}
      else throw error;
    } finally {
      if (showLoading) setLoading(false);
    }
  }, [requestBootstrap]);
	const refreshOverviewData = useCallback(() => load(false, 'overview'), [load]);
	const refreshAfterWrite = useCallback(async (scope: BootstrapScope, successMessage: string, tone: Toast['tone'] = 'success') => {
		try {
			await load(false, scope);
			notify(successMessage, tone);
		} catch (error) {
			notify(`${successMessage}；操作已经生效，但状态读回暂未完成：${getErrorMessage(error)}`, 'info');
			window.setTimeout(() => { void load(false, scope).catch(() => undefined); }, 3000);
		}
	}, [load, notify]);

	useEffect(() => {
		const showLoading = initialPageLoad.current;
		initialPageLoad.current = false;
		void load(showLoading, scopeForPage(page)).catch((error) => notify(`页面数据刷新失败：${getErrorMessage(error)}`, 'error'));
	}, [load, notify, page]);
	useEffect(() => {
		if (page !== 'overview') return;
		let disposed = false;
		const refresh = () => {
			if (disposed || document.visibilityState !== 'visible' || silentRefreshInFlight.current) return;
			silentRefreshInFlight.current = true;
			void refreshOverviewData().catch(() => undefined).finally(() => { silentRefreshInFlight.current = false; });
		};
		const interval = window.setInterval(refresh, 60_000);
		const onFocus = () => refresh();
		const onVisibility = () => { if (document.visibilityState === 'visible') refresh(); };
		window.addEventListener('focus', onFocus);
		document.addEventListener('visibilitychange', onVisibility);
		return () => {
			disposed = true;
			window.clearInterval(interval);
			window.removeEventListener('focus', onFocus);
			document.removeEventListener('visibilitychange', onVisibility);
		};
	}, [page, refreshOverviewData]);
  useEffect(() => {
    const handler = () => setPage(pageFromHash());
    window.addEventListener('hashchange', handler);
    return () => window.removeEventListener('hashchange', handler);
  }, []);

  const navigate = (next: PageId) => {
    setPage(next);
    window.history.replaceState(null, '', `#/${next}`);
  };

  const collectAll = async () => {
    setSyncing(true);
    try {
		const started = await api.collectAll();
		let job = started.data.job;
		setCollectionJob(job);
		notify(started.data.accepted ? `全局刷新已启动：共 ${job.total} 个上游` : '已有全局刷新正在运行，已接管当前进度', 'info');
		const deadline = Date.now() + 10 * 60_000;
		while (job.status === 'running' && Date.now() < deadline) {
			await new Promise((resolve) => window.setTimeout(resolve, 1000));
			job = (await api.collectionJob(job.id)).data;
			setCollectionJob(job);
		}
		if (job.status === 'running') throw new Error('全局刷新仍在后台运行，前端等待已达到 10 分钟；可稍后再次点击接管进度。');
		if (job.failed > 0) await refreshAfterWrite(scopeForPage(page), `全局刷新完成：成功 ${job.succeeded}，失败 ${job.failed}。失败详情已写入最近活动。`, 'error');
		else await refreshAfterWrite(scopeForPage(page), `全局刷新完成：${job.succeeded} 个上游均已更新`, 'success');
    } catch (error) {
      notify(getErrorMessage(error), 'error');
    } finally {
      setSyncing(false);
    }
  };

  const collectSupplier = async (id: string) => {
    setCollectingId(id);
    try {
		await api.collectSupplier(id);
		await refreshAfterWrite('overview', '上游采集完成');
    } catch (error) {
      notify(getErrorMessage(error), 'error');
    } finally {
      setCollectingId(null);
    }
  };

  const addSupplier = async (input: CreateSupplierInput) => {
    const result = await api.createSupplier(input);
    if (result.data.collection_error) {
		await refreshAfterWrite('overview', `上游已保存，首次采集需要处理：${result.data.collection_error}`, 'info');
    } else {
		await refreshAfterWrite('overview', '上游已保存并完成首次采集');
    }
  };

	const editSupplier = async (supplierId: string, input: UpdateSupplierInput) => {
		await api.updateSupplier(supplierId, input);
		await refreshAfterWrite('overview', '上游配置已保存并完成服务端读回');
	};

	const removeSupplier = async (supplierId: string, input: DeleteSupplierInput) => {
		await api.deleteSupplier(supplierId, input);
		await refreshAfterWrite('overview', '上游站点及其专属关联数据已删除；独立凭据已保留', 'success');
	};

  const togglePolicy = async (id: string, enabled: boolean) => {
	const previous = data?.policies.find((policy) => policy.id === id)?.enabled;
	if (previous === undefined || previous === enabled || togglingPolicyIds.has(id)) return;
	setTogglingPolicyIds((current) => new Set(current).add(id));
	setData((current) => current ? { ...current, policies: current.policies.map((policy) =>
		policy.id === id ? { ...policy, enabled } : policy) } : current);
    try {
	  const result = await api.togglePolicy(id, enabled);
	  setData((current) => current ? { ...current, policies: current.policies.map((policy) =>
		  policy.id === id ? { ...policy, enabled: result.data.enabled } : policy) } : current);
	  await refreshAfterWrite('pricing', enabled ? '规则已恢复运行' : '规则已暂停', 'info');
    } catch (error) {
	  setData((current) => current ? { ...current, policies: current.policies.map((policy) =>
		  policy.id === id ? { ...policy, enabled: previous } : policy) } : current);
      notify(getErrorMessage(error), 'error');
	} finally {
	  setTogglingPolicyIds((current) => {
		  const next = new Set(current);
		  next.delete(id);
		  return next;
	  });
    }
  };

	const updateAutomationControl = async (enabled: boolean) => {
		const result = await api.updateAutomationControl(enabled);
		await refreshAfterWrite('pricing', result.data.enabled ? '无人值守自动改价已开启' : '无人值守自动改价已暂停', result.data.enabled ? 'success' : 'info');
	};

  const updateProposal = async (id: string, status: 'applied' | 'rejected'): Promise<ProposalActionResult> => {
    try {
		let message: string;
		if (status === 'applied') {
			const result = await api.applyProposal(id);
			message = result.data.status === 'no_change'
				? '目标倍率已经符合规则，无需重复写入'
				: `变更已应用并完成读回验证${result.data.readback_value ? `：${result.data.readback_value}` : ''}`;
		} else {
			await api.rejectProposal(id);
			message = '变更建议已拒绝';
		}
		await refreshAfterWrite('changes', message, status === 'applied' ? 'success' : 'info');
		return { ok: true, message };
    } catch (error) {
		await load(false, 'changes').catch(() => undefined);
		const detail = getErrorMessage(error);
		const message = error instanceof ApiError && error.status === 409
			? `未执行：${detail}。系统已按最新证据重新生成建议。`
			: `执行失败：${detail}`;
      notify(message, 'error');
		return { ok: false, message };
    }
  };

	const rollbackProposal = async (id: string): Promise<ProposalActionResult> => {
		try {
			const result = await api.rollbackProposal(id);
			const message = result.data.status === 'rolled_back' ? '变更已安全撤回并完成目标读回' : `撤回结果：${result.data.status}`;
			await refreshAfterWrite('changes', message, 'success');
			return { ok: true, message };
		} catch (error) {
			await load(false, 'changes').catch(() => undefined);
			const message = `撤回未执行：${getErrorMessage(error)}（若目标已被人工修改，系统会拒绝覆盖）`;
			notify(message, 'error');
			return { ok: false, message };
		}
	};

  const savePolicy = async (input: CreatePricingPolicyInput, policyId?: string, syncTargetWriteMode = false): Promise<{ warning?: string }> => {
    const savedPolicy = policyId ? await api.updatePolicy(policyId, input) : await api.createPolicy(input);
    try {
		let targetWriteMode = 'approval';
		const targets = await api.listTargets();
		const target = targets.data.find((item) => item.id === input.target_site_id);
		if (!target) throw new Error('规则已保存，但没有找到对应的本站目标，请重新连接目标站点。');
		targetWriteMode = target.write_mode;
		if (input.execution_mode === 'auto' && syncTargetWriteMode && target.write_mode !== 'auto') {
			const updated = await api.updateTarget(target.id, {
				name: target.name, base_url: target.base_url, adapter: target.adapter,
				credential_id: target.credential_id, enabled: target.enabled, write_mode: 'auto',
			});
			targetWriteMode = updated.data.write_mode;
		}
		const simulation = await api.simulatePolicy(savedPolicy.data.id);
		const proposal = await api.proposePolicy(savedPolicy.data.id);
		if (input.execution_mode === 'auto') {
			if (proposal.data.mode !== 'auto' || !simulation.data.calculation.automation_eligible) {
				const reason = simulation.data.calculation.automation_reason || '当前证据尚未达到无人值守写入门槛';
				throw new Error(`规则已保存，但本次保持人工确认：${reason}`);
			}
			if (targetWriteMode !== 'auto') {
				throw new Error('规则已保存为自动应用，但本站目标仍处于“仅人工确认”；请勾选同步开启目标自动写入后再次保存。');
			}
			if (proposal.data.status !== 'applied' && proposal.data.status !== 'no_change') {
				await api.applyProposal(String(proposal.data.id));
			}
		}
		await refreshAfterWrite('pricing', input.execution_mode === 'auto'
			? (policyId ? '规则已更新，并完成自动写入与读回验证' : '规则已保存，并完成自动写入与读回验证')
			: (policyId ? '规则已更新，模拟结果和变更建议已重新生成' : '规则已保存，模拟结果和变更建议已生成'));
      return {};
    } catch (error) {
	  await load(false, 'pricing').catch(() => undefined);
		const detail = getErrorMessage(error);
		const warning = detail.startsWith('规则已保存')
			? detail
			: `规则已经${policyId ? '更新' : '保存'}，但后续校验未完成：${detail}`;
      notify(warning, 'error');
      return { warning };
    }
  };

	const verifyRecoveryToken = async () => {
		if (!recoveryToken.trim()) {
			setFatalError('请输入管理令牌。');
			return;
		}
		setAuthSubmitting(true);
		setFatalError('');
		api.setAdminToken(recoveryToken);
		try {
			await verifyApiContract();
			const result = await api.bootstrap(scopeForPage(page));
			setData(result.data);
			setFatalStatus(0);
			notify('管理令牌已验证', 'info');
		} catch (error) {
			setData(null);
			setFatalError(getErrorMessage(error));
			setFatalStatus(error instanceof ApiError ? error.status : 0);
		} finally {
			setAuthSubmitting(false);
		}
	};

  const importConnector = async (input: ConnectorSpecInput) => {
		await api.createConnectorSpec(input);
		await refreshAfterWrite('connectors', 'JSON Connector 已导入并启用');
  };

  const saveAdminToken = async (token: string) => {
    api.setAdminToken(token);
	await refreshAfterWrite(scopeForPage(page), token.trim() ? '管理令牌已在当前标签页验证' : '管理令牌已清除', 'info');
  };

  const startPairing = async (supplierId: string) => {
    const result = await api.startPairing(supplierId);
    return result.data.code;
  };

	const saveSupplierCredential = async (supplierId: string, input: CreateCredentialInput) => {
		const suppliers = await api.listSuppliers();
		const supplier = suppliers.data.find((item) => String(item.id) === supplierId);
		if (!supplier) throw new Error('没有找到对应上游，请刷新页面后重试。');
		const credential = await api.createCredential(input);
		await api.updateSupplier(supplier.id, {
			name: supplier.name, base_url: supplier.base_url, adapter: supplier.adapter,
			pricing_path: supplier.pricing_path ?? '', balance_path: supplier.balance_path ?? '',
			credential_id: credential.data.id, enabled: supplier.enabled,
			poll_interval_seconds: supplier.poll_interval_seconds, recharge_value_factor: supplier.recharge_value_factor ?? '1', focus_groups: supplier.focus_groups ?? [],
			focus_models: supplier.focus_models ?? [], metadata: supplier.metadata ?? {},
		});
		await api.collectSupplier(supplierId);
		await refreshAfterWrite('connectors', '凭据已加密保存、关联站点并完成验证采集。');
	};

	const saveSupplierRechargeFactor = async (supplierId: string, factor: string) => {
		const suppliers = await api.listSuppliers();
		const supplier = suppliers.data.find((item) => String(item.id) === supplierId);
		if (!supplier) throw new Error('没有找到对应上游，请刷新页面后重试。');
		await api.updateSupplier(supplier.id, {
			name: supplier.name, base_url: supplier.base_url, adapter: supplier.adapter,
			pricing_path: supplier.pricing_path ?? '', balance_path: supplier.balance_path ?? '',
			credential_id: supplier.credential_id ?? null, enabled: supplier.enabled,
			poll_interval_seconds: supplier.poll_interval_seconds, recharge_value_factor: factor,
			focus_groups: supplier.focus_groups ?? [], focus_models: supplier.focus_models ?? [], metadata: supplier.metadata ?? {},
		});
		await refreshAfterWrite('overview', `充值折算系数已更新为 ${factor}，所有预览和未执行建议已按现金成本实时重算。`);
	};

  const pendingCount = useMemo(() => data?.proposals.filter((item) => item.status === 'pending').length ?? 0, [data]);

  if (loading) return <LoadingState />;
	if (contractMismatch && !data) return <ErrorState message={`页面版本 ${contractMismatch.expected} 与服务端版本 ${contractMismatch.actual} 不一致；旧数据和写操作已停止。`} onRetry={() => window.location.reload()} />;
	if ((fatalStatus === 401 || fatalStatus === 403) && !data) return <AuthGate token={recoveryToken} message={fatalError} submitting={authSubmitting} onTokenChange={setRecoveryToken} onSubmit={verifyRecoveryToken} />;
  if (fatalError || !data) return <ErrorState message={fatalError || '控制台没有返回可用数据'} onRetry={() => void load(true, scopeForPage(page))} />;

  const content = (() => {
    switch (page) {
      case 'suppliers': return <SuppliersPage suppliers={data.suppliers} collectingId={collectingId} onCollect={collectSupplier} onAdd={addSupplier} onEdit={editSupplier} onDelete={removeSupplier} onSaveRechargeFactor={saveSupplierRechargeFactor} onOpenCredentials={(supplierId, action) => {
		setConnectorIntent({ supplierId, action, requestId: `${supplierId}:${action}:${Date.now()}` });
		navigate('connectors');
	  }} />;
	  case 'watchlist': return <WatchlistPage suppliers={data.suppliers} onChanged={() => load(false, 'overview')} onNotify={notify} />;
	  case 'wallboard': return <WallboardPage data={data} onNavigate={navigate} onRefresh={refreshOverviewData} />;
	      case 'pricing': return <PricingPage policies={data.policies} automationEnabled={data.automationEnabled} backgroundWorkersEnabled={data.backgroundWorkersEnabled} remoteWritesEnabled={data.remoteWritesEnabled} togglingPolicyIds={togglingPolicyIds} onAutomationChange={updateAutomationControl} onToggle={togglePolicy} onSavePolicy={savePolicy} onReload={() => load(false, 'pricing')} onOpenMirror={() => navigate('mirror')} />;
			case 'mirror': return <MirrorManagementPage onNotify={notify} />;
			case 'mapping-drafts': return <MappingDraftsPage onReload={() => load(false, 'overview')} onNotify={notify} onOpenMirror={() => navigate('mirror')} />;
      case 'balances': return <BalancesPage balances={data.balances} />;
	      case 'discoveries': return <DiscoveriesPage discoveries={data.discoveries} onOpenMirror={() => navigate('mirror')} />;
		case 'intelligence': return <IntelligencePage onNavigate={navigate} onNotify={notify} />;
	  case 'changes': return <ChangesPage proposals={data.proposals} activities={data.activities} onApply={(id) => updateProposal(id, 'applied')} onReject={(id) => updateProposal(id, 'rejected')} onRollback={rollbackProposal} />;
      case 'connectors': return <ConnectorsPage connectors={data.connectors} credentials={data.credentials} suppliers={data.suppliers} intent={connectorIntent} onIntentConsumed={(requestId) => setConnectorIntent((current) => current?.requestId === requestId ? null : current)} onPair={startPairing} onPairVerified={() => load(false, 'connectors')} onSaveCredential={saveSupplierCredential} onImportConnector={importConnector} onReload={() => load(false, 'connectors')} adminToken={api.getAdminToken()} onSaveAdminToken={saveAdminToken} onNotify={(message) => notify(message)} />;
		case 'config-portability': return <ConfigPortabilityPage />;
      default: return <OverviewPage data={data} onNavigate={navigate} />;
    }
  })();

  return (
    <>
      <Shell page={page} onPageChange={navigate} syncing={syncing} syncProgress={collectionJob && collectionJob.status === 'running' ? `${collectionJob.completed}/${collectionJob.total}` : ''} onSync={collectAll} pendingChanges={pendingCount} contractMismatch={contractMismatch} onReloadVersion={() => window.location.reload()}>
		{page !== 'wallboard' && !data.remoteWritesEnabled && <div className="remote-write-guard-banner" role="status"><ShieldCheck size={18} /><div><strong>本地验收候选禁止远程写入</strong><span>目录采集、目标读取、模拟和预览仍可使用；改价、回滚、上架、渠道测试、登录和续约均由服务端硬门禁阻止。</span></div></div>}
		{content}
	  </Shell>
      <div className="toast-stack" aria-live="polite">
        {toasts.map((toast) => <div className={`toast toast-${toast.tone}`} key={toast.id}>{toast.tone === 'success' ? <CheckCircle2 size={18} /> : toast.tone === 'error' ? <XCircle size={18} /> : <Info size={18} />}<span>{toast.message}</span></div>)}
      </div>
    </>
  );
}
