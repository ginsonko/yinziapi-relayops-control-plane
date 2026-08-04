import type {
  ApiResult,
	ActivityPage,
	ActivityQuery,
	BootstrapData,
  CatalogSnapshot,
	ConnectTargetInput,
	ConnectTargetResult,
	CollectionAttempt,
	CollectionJob,
  ConnectorSpecInput,
	CreateCredentialInput,
  CreatePricingPolicyInput,
  CreateSupplierInput,
	ApplyListingPlanInput,
	ListingChangeSet,
	ListingPlan,
	ListingPlanRequest,
	ConfigImportMode,
	ConfigImportPreview,
	ConfigImportRun,
	PortableConfig,
	PortableConfigExport,
	RawCredential,
	PolicySimulationResult,
	PricingPolicyBulkMutationInput,
	PricingPolicyBulkPreview,
	PricingPolicyBulkResult,
	RawPricingPolicy,
	RelayOpsHealth,
  RawSupplier,
  TargetSite,
	TargetChannel,
	TargetChannelPreview,
	TargetChannelRoutingUpdate,
	TargetCapabilityMatrix,
	TargetControlOperation,
	TargetPricingOptionPreview,
	TargetSnapshot,
	OpportunityResponse,
	IntelligenceConfig,
	IntelligenceDashboard,
	OverviewIntelligenceSnapshot,
	ReauthorizeTargetInput,
	AuthCapabilityEvidence,
	BrowserCustodyStatus,
	ConfigureCredentialRenewalInput,
	ConnectorManifestDocument,
	ConnectorManifestDeletionImpact,
	ConnectorManifestRecord,
	ConnectorSpecDeletionImpact,
	ConnectorSpecRecord,
	CredentialRenewalDetail,
	CredentialRenewalPreset,
	PairingStatus,
	RawChangeProposal,
	DeleteSupplierInput,
	SupplierDeletionImpact,
	UpdateSupplierInput,
	UpsertTargetInput,
	SupplierManifestBinding,
	SupplierPasswordLoginInput,
	SupplierPasswordLoginResult,
	MappingDraftPreviewInput,
	MappingDraftPreview,
	MappingDraftMaterializeInput,
	MappingDraftMaterializeResult,
	ManualMappingDraftPreviewInput,
	ManualMappingDraftPreview,
	ManualMappingDraftMaterializeInput,
	ManualMappingDraftMaterializeResult,
	TargetLedgerSyncConfig,
	TargetLedgerSyncResult,
	TargetLedgerSyncStatus,
	AutomationControl,
	AutomationConfirmationConfig,
	AutomationSafetyConfig,
	MirrorApplyReceipt,
	MirrorAdoptionPreview,
	MirrorAdoptionPreviewRequest,
	MirrorAdoptionReceipt,
	MirrorChangeEvent,
	MirrorChangeSet,
	MirrorPolicy,
	MirrorPolicyDocument,
	MirrorPreview,
	DeleteTargetInput,
	TargetDeletionImpact,
	DeletePricingPolicyInput,
	PricingPolicyDeletionImpact,
	CredentialDeletionImpact,
	DeleteCredentialInput,
	DeleteConnectorObjectInput,
	UpdateConnectorSpecInput,
	DeleteActivitiesInput,
	MirrorDraftBatchInput,
	WatchItemInput,
	WatchlistResponse,
	WatchSettings,
} from './types';
import type { BootstrapScope } from './bootstrap';

const API_BASE = '/api/v1';
const TOKEN_KEY = 'relayops_admin_token';
const API_CONTRACT_HEADER = 'X-RelayOps-API-Contract';
const API_CONTRACT_EVENT = 'relayops:api-contract-mismatch';

export const EXPECTED_API_CONTRACT = 'relayops-v3-r49-4-settings-busy-retry-20260804';

export type ApiContractMismatchDetail = {
	expected: string;
	actual: string;
};

export function reportApiContractMismatch(actual: string) {
	const detail: ApiContractMismatchDetail = {
		expected: EXPECTED_API_CONTRACT,
		actual: actual.trim() || '旧版/未知版本',
	};
	window.dispatchEvent(new CustomEvent<ApiContractMismatchDetail>(API_CONTRACT_EVENT, { detail }));
	return detail;
}

export function onApiContractMismatch(listener: (detail: ApiContractMismatchDetail) => void) {
	const handler = (event: Event) => listener((event as CustomEvent<ApiContractMismatchDetail>).detail);
	window.addEventListener(API_CONTRACT_EVENT, handler);
	return () => window.removeEventListener(API_CONTRACT_EVENT, handler);
}

// Older previews persisted the management secret across browser restarts.
// Remove that legacy copy and keep the token scoped to the current tab only.
window.localStorage.removeItem(TOKEN_KEY);

export class ApiError extends Error {
  constructor(message: string, readonly status: number, readonly details?: unknown) {
    super(message);
    this.name = 'ApiError';
  }
}

type RequestOptions<T> = {
  timeoutMs?: number;
	signal?: AbortSignal;
};

function adminToken() {
  return window.sessionStorage.getItem(TOKEN_KEY)?.trim() ?? '';
}

async function parseResponse(response: Response) {
  const text = await response.text();
  if (!text.trim()) return undefined;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    throw new ApiError(`服务返回了无法解析的数据（HTTP ${response.status}）`, response.status, text.slice(0, 300));
  }
}

async function request<T>(path: string, init: RequestInit = {}, options: RequestOptions<T> = {}): Promise<ApiResult<T>> {
  const controller = new AbortController();
  const timeoutMs = options.timeoutMs ?? 15_000;
  const timeout = window.setTimeout(() => controller.abort(), timeoutMs);
	const abortFromCaller = () => controller.abort(options.signal?.reason);
	if (options.signal?.aborted) abortFromCaller();
	else options.signal?.addEventListener('abort', abortFromCaller, { once: true });

  try {
    const token = adminToken();
    const response = await fetch(`${API_BASE}${path}`, {
      ...init,
      headers: {
        'Content-Type': 'application/json',
		[API_CONTRACT_HEADER]: EXPECTED_API_CONTRACT,
        ...(token ? { 'X-RelayOps-Token': token } : {}),
        ...init.headers,
      },
      signal: controller.signal,
    });
	const responseContract = response.headers.get(API_CONTRACT_HEADER)?.trim();
	if (responseContract && responseContract !== EXPECTED_API_CONTRACT) {
		const mismatch = reportApiContractMismatch(responseContract);
		throw new ApiError(`控制台版本已更新：当前页面使用 ${mismatch.expected}，服务端使用 ${mismatch.actual}。请重新加载新版本后再操作`, 409, mismatch);
	}
    const payload = await parseResponse(response) as { data?: T; message?: string; success?: boolean } | T | undefined;
    if (!response.ok) {
      const directMessage = payload && typeof payload === 'object' && 'message' in payload && typeof payload.message === 'string'
        ? payload.message : '';
      const nestedError = payload && typeof payload === 'object' && 'data' in payload
        && payload.data && typeof payload.data === 'object' && 'error' in payload.data
        && typeof payload.data.error === 'string' ? payload.data.error : '';
      const nestedMessage = payload && typeof payload === 'object' && 'data' in payload
        && payload.data && typeof payload.data === 'object' && 'message' in payload.data
        && typeof payload.data.message === 'string' ? payload.data.message : '';
      const message = directMessage || nestedMessage || nestedError || `请求失败（HTTP ${response.status}）`;
      throw new ApiError(message, response.status, payload);
    }
    const data = payload && typeof payload === 'object' && 'data' in payload
      ? (payload as { data: T }).data
      : payload as T;
    return { data, source: 'live' };
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') {
      throw new ApiError(`请求超过 ${Math.round(timeoutMs / 1000)} 秒仍未完成，请检查上游网络后重试`, 408);
    }
    throw error;
  } finally {
    window.clearTimeout(timeout);
		options.signal?.removeEventListener('abort', abortFromCaller);
  }
}

export function getErrorMessage(error: unknown) {
  return error instanceof Error ? error.message : '操作未完成，请稍后重试';
}

export const api = {
  getAdminToken: adminToken,
  setAdminToken: (token: string) => {
    const value = token.trim();
    if (value) window.sessionStorage.setItem(TOKEN_KEY, value);
    else window.sessionStorage.removeItem(TOKEN_KEY);
  },
  bootstrap: (scope: BootstrapScope = 'full') => request<BootstrapData>(
		scope === 'full' ? '/console/bootstrap' : `/console/bootstrap?scope=${encodeURIComponent(scope)}`, {}, { timeoutMs: 30_000 },
	),
	watchlist: () => request<WatchlistResponse>('/watchlist', {}, { timeoutMs: 60_000 }),
	createWatchItem: (input: WatchItemInput) => request('/watchlist/items', { method: 'POST', body: JSON.stringify(input) }),
	updateWatchItem: (id: string, input: WatchItemInput) => request(`/watchlist/items/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(input) }),
	deleteWatchItem: (id: string, input: WatchItemInput) => request(`/watchlist/items/${encodeURIComponent(id)}`, { method: 'DELETE', body: JSON.stringify(input) }),
	reorderWatchItems: (ids: number[]) => request('/watchlist/order', { method: 'PUT', body: JSON.stringify({ ids }) }),
	updateWatchSettings: (settings: WatchSettings) => request<WatchSettings>('/watchlist/settings', { method: 'PUT', body: JSON.stringify(settings) }),
	listActivities: (input: ActivityQuery = {}) => {
		const query = new URLSearchParams();
		for (const [key, value] of Object.entries(input)) if (value !== undefined && value !== '') query.set(key, String(value));
		return request<ActivityPage>(`/activities${query.size ? `?${query.toString()}` : ''}`);
	},
	deleteActivities: (input: DeleteActivitiesInput) => request<{ deleted: number; limit: number }>(
		'/activities', { method: 'DELETE', body: JSON.stringify(input) },
	),
	recordMirrorDraftBatch: (input: MirrorDraftBatchInput) => request<{ recorded: boolean }>(
		'/activities/mirror-draft-batch', { method: 'POST', body: JSON.stringify(input) },
	),
	health: () => request<RelayOpsHealth>('/health', {}, { timeoutMs: 5_000 }),
	automationControl: () => request<AutomationControl>('/automation-control'),
	updateAutomationControl: (enabled: boolean) => request<AutomationControl>(
		'/automation-control', { method: 'PUT', body: JSON.stringify({ enabled }) },
	),
	automationConfirmation: () => request<AutomationConfirmationConfig>('/automation-confirmation'),
	updateAutomationConfirmation: (input: Partial<AutomationConfirmationConfig>) => request<AutomationConfirmationConfig>(
		'/automation-confirmation', { method: 'PUT', body: JSON.stringify(input) },
	),
	automationSafety: () => request<AutomationSafetyConfig>('/automation-safety'),
	updateAutomationSafety: (input: Partial<AutomationSafetyConfig>) => request<AutomationSafetyConfig>(
		'/automation-safety', { method: 'PUT', body: JSON.stringify(input) },
	),
	opportunities: (refresh = false, targetId?: number) => {
		const query = new URLSearchParams();
		if (targetId !== undefined && targetId > 0) query.set('target_id', String(targetId));
		if (refresh) query.set('refresh', 'true');
		const suffix = query.toString() ? `?${query.toString()}` : '';
		return request<OpportunityResponse>(`/opportunities${suffix}`, {}, { timeoutMs: 60_000 });
	},
	intelligence: () => request<IntelligenceDashboard>('/intelligence', {}, { timeoutMs: 90_000 }),
	overviewIntelligence: () => request<OverviewIntelligenceSnapshot>('/intelligence/overview', {}, { timeoutMs: 60_000 }),
	intelligenceConfig: () => request<IntelligenceConfig>('/intelligence/config'),
	updateIntelligenceConfig: (input: IntelligenceConfig) => request<IntelligenceConfig>(
		'/intelligence/config', { method: 'PUT', body: JSON.stringify(input) },
	),
  listSuppliers: (signal?: AbortSignal) => request<RawSupplier[]>('/suppliers', {}, { signal }),
	listCredentials: (signal?: AbortSignal) => request<RawCredential[]>('/credentials', {}, { signal }),
  listTargets: (signal?: AbortSignal) => request<TargetSite[]>('/targets', {}, { signal }),
	createTarget: (input: UpsertTargetInput) => request<TargetSite>(
		'/targets', { method: 'POST', body: JSON.stringify(input) },
	),
	connectTarget: (input: ConnectTargetInput) => request<ConnectTargetResult>(
		'/targets/connect', { method: 'POST', body: JSON.stringify(input) }, { timeoutMs: 60_000 },
	),
	updateTarget: (targetId: string | number, input: UpsertTargetInput) => request<TargetSite>(
		`/targets/${targetId}`, { method: 'PUT', body: JSON.stringify(input) },
	),
	targetDeletionImpact: (targetId: string | number) => request<TargetDeletionImpact>(
		`/targets/${targetId}/deletion-impact`,
	),
	deleteTarget: (targetId: string | number, input: DeleteTargetInput) => request<{ deleted: boolean }>(
		`/targets/${targetId}`, { method: 'DELETE', body: JSON.stringify(input) },
	),
  supplierSnapshot: (supplierId: string | number, signal?: AbortSignal) => request<CatalogSnapshot>(
    `/suppliers/${supplierId}/snapshot`,
    {},
		{ signal },
  ),
	supplierAttempts: (supplierId: string | number, limit = 20) => request<CollectionAttempt[]>(
		`/suppliers/${supplierId}/attempts?limit=${encodeURIComponent(String(limit))}`,
	),
	supplierCapabilities: (supplierId: string | number) => request<AuthCapabilityEvidence[]>(
		`/suppliers/${supplierId}/capabilities`,
	),
  targetSnapshot: (targetId: string | number, signal?: AbortSignal) => request<TargetSnapshot>(
    `/targets/${targetId}/test`,
    { method: 'POST' },
		{ timeoutMs: 45_000, signal },
  ),
	targetCatalog: (targetId: string | number) => request<CatalogSnapshot>(`/targets/${targetId}/catalog`, {}, { timeoutMs: 60_000 }),
	listTargetChannels: (targetId: string | number) => request<TargetChannel[]>(`/targets/${targetId}/channels`, {}, { timeoutMs: 45_000 }),
	targetCapabilities: (targetId: string | number) => request<TargetCapabilityMatrix>(`/targets/${targetId}/capabilities`, {}, { timeoutMs: 60_000 }),
	targetOperations: (targetId: string | number, limit = 30) => request<TargetControlOperation[]>(`/targets/${targetId}/operations?limit=${encodeURIComponent(String(limit))}`),
	targetLedgerSync: (targetId: string | number) => request<TargetLedgerSyncStatus>(`/targets/${targetId}/ledger-sync`),
	updateTargetLedgerSync: (targetId: string | number, input: TargetLedgerSyncConfig) => request<TargetLedgerSyncConfig>(
		`/targets/${targetId}/ledger-sync`, { method: 'PUT', body: JSON.stringify(input) },
	),
	runTargetLedgerSync: (targetId: string | number) => request<TargetLedgerSyncResult>(
		`/targets/${targetId}/ledger-sync/run`, { method: 'POST' }, { timeoutMs: 120_000 },
	),
	previewTargetPricingOption: (targetId: string | number, option: string, values: Record<string, string>) => request<TargetPricingOptionPreview>(
		`/targets/${targetId}/pricing-options/${encodeURIComponent(option)}/preview`, { method: 'POST', body: JSON.stringify({ values }) }, { timeoutMs: 60_000 },
	),
	applyTargetPricingOption: (targetId: string | number, option: string, input: { expected_hash: string; preview_hash: string; values: Record<string, string>; idempotency_key: string; confirm: true }) => request<TargetControlOperation>(
		`/targets/${targetId}/pricing-options/${encodeURIComponent(option)}/apply`, { method: 'POST', body: JSON.stringify(input) }, { timeoutMs: 90_000 },
	),
	previewTargetChannelRouting: (targetId: string | number, channelId: string | number, routing: TargetChannelRoutingUpdate) => request<TargetChannelPreview>(
		`/targets/${targetId}/channels/${channelId}/routing/preview`, { method: 'POST', body: JSON.stringify({ routing }) }, { timeoutMs: 60_000 },
	),
	applyTargetChannelRouting: (targetId: string | number, channelId: string | number, input: { expected_hash: string; preview_hash: string; routing: TargetChannelRoutingUpdate; idempotency_key: string; confirm: true }) => request<TargetControlOperation>(
		`/targets/${targetId}/channels/${channelId}/routing/apply`, { method: 'POST', body: JSON.stringify(input) }, { timeoutMs: 90_000 },
	),
	previewTargetChannelStatus: (targetId: string | number, channelId: string | number, status: number) => request<TargetChannelPreview>(
		`/targets/${targetId}/channels/${channelId}/status/preview`, { method: 'POST', body: JSON.stringify({ status }) }, { timeoutMs: 60_000 },
	),
	applyTargetChannelStatus: (targetId: string | number, channelId: string | number, input: { expected_hash: string; preview_hash: string; status: number; idempotency_key: string; confirm: true }) => request<TargetControlOperation>(
		`/targets/${targetId}/channels/${channelId}/status/apply`, { method: 'POST', body: JSON.stringify(input) }, { timeoutMs: 90_000 },
	),
	controlledTestTargetChannel: (targetId: string | number, channelId: string | number, input: { model: string; timeout_seconds: number; acknowledge_cost: true; idempotency_key: string }) => request<TargetControlOperation>(
		`/targets/${targetId}/channels/${channelId}/test`, { method: 'POST', body: JSON.stringify(input) }, { timeoutMs: Math.max(65_000, input.timeout_seconds * 1000 + 15_000) },
	),
	reauthorizeTarget: (targetId: string | number, input: ReauthorizeTargetInput) => request<{ target: TargetSite; credential: RawCredential; capabilities: TargetCapabilityMatrix }>(
		`/targets/${targetId}/reauthorize`, { method: 'POST', body: JSON.stringify(input) }, { timeoutMs: 90_000 },
	),
  createSupplier: (input: CreateSupplierInput) => request<{ supplier: RawSupplier; collection?: unknown; collection_error?: string }>(
    '/suppliers',
    { method: 'POST', body: JSON.stringify(input) },
    { timeoutMs: 95_000 },
  ),
	updateSupplier: (supplierId: string | number, input: UpdateSupplierInput) => request<RawSupplier>(
		`/suppliers/${supplierId}`,
		{ method: 'PUT', body: JSON.stringify(input) },
	),
	supplierDeletionImpact: (supplierId: string | number) => request<SupplierDeletionImpact>(
		`/suppliers/${supplierId}/deletion-impact`,
	),
	deleteSupplier: (supplierId: string | number, input: DeleteSupplierInput) => request<{ deleted: boolean }>(
		`/suppliers/${supplierId}`,
		{ method: 'DELETE', body: JSON.stringify(input) },
	),
	createCredential: (input: CreateCredentialInput) => request<RawCredential>(
		'/credentials',
		{ method: 'POST', body: JSON.stringify(input) },
	),
	credentialDeletionImpact: (credentialId: string | number) => request<CredentialDeletionImpact>(
		`/credentials/${credentialId}/deletion-impact`,
	),
	deleteCredential: (credentialId: string | number, input: DeleteCredentialInput) => request<{ deleted: boolean }>(
		`/credentials/${credentialId}`, { method: 'DELETE', body: JSON.stringify(input) },
	),
  collectSupplier: (supplierId: string) => request<unknown>(
    `/suppliers/${supplierId}/collect?wait=true`,
    { method: 'POST' },
    { timeoutMs: 95_000 },
  ),
	collectAll: () => request<{ accepted: boolean; job: CollectionJob }>('/suppliers/collect', { method: 'POST' }),
	collectionJob: (jobId: string) => request<CollectionJob>(`/collection-jobs/${encodeURIComponent(jobId)}`),
  togglePolicy: (policyId: string, enabled: boolean) => request<{ enabled: boolean }>(
    `/pricing-policies/${policyId}`,
    { method: 'PATCH', body: JSON.stringify({ enabled }) },
  ),
	previewPolicyBulkMutation: (input: PricingPolicyBulkMutationInput) => request<PricingPolicyBulkPreview>(
		'/pricing-policies/bulk/preview',
		{ method: 'POST', body: JSON.stringify(input) },
	),
	applyPolicyBulkMutation: (input: PricingPolicyBulkMutationInput) => request<PricingPolicyBulkResult>(
		'/pricing-policies/bulk/apply',
		{ method: 'POST', body: JSON.stringify(input) },
	),
	previewMappingDrafts: (input: MappingDraftPreviewInput) => request<MappingDraftPreview>(
		'/mapping-drafts/preview',
		{ method: 'POST', body: JSON.stringify(input) },
		{ timeoutMs: 90_000 },
	),
	materializeMappingDrafts: (input: MappingDraftMaterializeInput) => request<MappingDraftMaterializeResult>(
		'/mapping-drafts/materialize',
		{ method: 'POST', body: JSON.stringify(input) },
		{ timeoutMs: 90_000 },
	),
	previewManualMappingDrafts: (input: ManualMappingDraftPreviewInput) => request<ManualMappingDraftPreview>(
		'/mapping-drafts/manual/preview',
		{ method: 'POST', body: JSON.stringify(input) },
		{ timeoutMs: 90_000 },
	),
	materializeManualMappingDrafts: (input: ManualMappingDraftMaterializeInput) => request<ManualMappingDraftMaterializeResult>(
		'/mapping-drafts/manual/materialize',
		{ method: 'POST', body: JSON.stringify(input) },
		{ timeoutMs: 90_000 },
	),
  createPolicy: (input: CreatePricingPolicyInput) => request<RawPricingPolicy>(
    '/pricing-policies',
    { method: 'POST', body: JSON.stringify(input) },
  ),
	listPolicies: () => request<RawPricingPolicy[]>('/pricing-policies'),
	updatePolicy: (policyId: string | number, input: CreatePricingPolicyInput) => request<RawPricingPolicy>(
		`/pricing-policies/${policyId}`,
		{ method: 'PUT', body: JSON.stringify(input) },
	),
	pricingPolicyDeletionImpact: (policyId: string | number) => request<PricingPolicyDeletionImpact>(
		`/pricing-policies/${policyId}/deletion-impact`,
	),
	deletePricingPolicy: (policyId: string | number, input: DeletePricingPolicyInput) => request<{ deleted: boolean }>(
		`/pricing-policies/${policyId}`, { method: 'DELETE', body: JSON.stringify(input) },
	),
  simulatePolicy: (policyId: string | number) => request<PolicySimulationResult>(
    `/pricing-policies/${policyId}/simulate`,
    { method: 'POST' },
    { timeoutMs: 45_000 },
  ),
	proposePolicy: (policyId: string | number) => request<RawChangeProposal>(
    `/pricing-policies/${policyId}/propose`,
    { method: 'POST' },
    { timeoutMs: 45_000 },
  ),
  applyProposal: (proposalId: string) => request<RawChangeProposal>(
    `/change-proposals/${proposalId}/apply`,
    { method: 'POST' },
    { timeoutMs: 60_000 },
  ),
	rollbackProposal: (proposalId: string) => request<RawChangeProposal>(
		`/change-proposals/${proposalId}/rollback`, { method: 'POST' }, { timeoutMs: 60_000 },
	),
  rejectProposal: (proposalId: string) => request<{ status: 'rejected' }>(
    `/change-proposals/${proposalId}/reject`,
    { method: 'POST' },
  ),
  startPairing: (supplierId: string) => request<{ code: string; expiresIn: number }>(
    `/credentials/${supplierId}/pair`,
    { method: 'POST' },
    { timeoutMs: 60_000 },
  ),
	pairingStatus: (code: string) => request<PairingStatus>(
		`/pair/${encodeURIComponent(code)}/status`,
	),
	loginSupplier: (supplierId: string | number, input: SupplierPasswordLoginInput) => request<SupplierPasswordLoginResult>(
		`/suppliers/${supplierId}/login`, { method: 'POST', body: JSON.stringify(input) }, { timeoutMs: 90_000 },
	),
	listConnectorManifests: () => request<ConnectorManifestRecord[]>('/connector-manifests'),
	importConnectorManifest: (manifest: ConnectorManifestDocument) => request<ConnectorManifestRecord>(
		'/connector-manifests', { method: 'POST', body: JSON.stringify(manifest) },
	),
	updateConnectorManifest: (manifestId: string | number, manifest: ConnectorManifestDocument, expectedUpdatedAt: string) => request<ConnectorManifestRecord>(
		`/connector-manifests/${manifestId}`, { method: 'PUT', body: JSON.stringify({ manifest, expected_updated_at: expectedUpdatedAt }) },
	),
	connectorManifestDeletionImpact: (manifestId: string | number) => request<ConnectorManifestDeletionImpact>(
		`/connector-manifests/${manifestId}/deletion-impact`,
	),
	deleteConnectorManifest: (manifestId: string | number, input: DeleteConnectorObjectInput) => request<{ deleted: boolean }>(
		`/connector-manifests/${manifestId}`, { method: 'DELETE', body: JSON.stringify(input) },
	),
	getSupplierConnectorManifest: (supplierId: string | number) => request<SupplierManifestBinding>(
		`/suppliers/${supplierId}/connector-manifest`,
	),
	bindSupplierConnectorManifest: (supplierId: string | number, manifestId: number | null) => request<SupplierManifestBinding | { supplier_id: number; manifest_id: null }>(
		`/suppliers/${supplierId}/connector-manifest`, { method: 'PUT', body: JSON.stringify({ manifest_id: manifestId }) },
	),
	credentialRenewalPreset: (supplierId: string | number) => request<CredentialRenewalPreset>(
		`/suppliers/${supplierId}/credential-renewal-preset`,
	),
	credentialRenewal: (credentialId: string | number) => request<CredentialRenewalDetail>(
		`/credentials/${credentialId}/renewal`,
	),
	configureCredentialRenewal: (credentialId: string | number, input: ConfigureCredentialRenewalInput) => request<CredentialRenewalDetail>(
		`/credentials/${credentialId}/renewal`, { method: 'PUT', body: JSON.stringify(input) }, { timeoutMs: 45_000 },
	),
	removeCredentialRenewal: (credentialId: string | number) => request<CredentialRenewalDetail>(
		`/credentials/${credentialId}/renewal`, { method: 'DELETE' },
	),
	triggerCredentialRenewal: (credentialId: string | number) => request<CredentialRenewalDetail>(
		`/credentials/${credentialId}/renewal/trigger`, { method: 'POST' }, { timeoutMs: 60_000 },
	),
	browserCustody: (supplierId: string | number) => request<BrowserCustodyStatus>(
		`/suppliers/${supplierId}/browser-custody`,
	),
	revokeBrowserCustody: (supplierId: string | number) => request<{ revoked: boolean }>(
		`/suppliers/${supplierId}/browser-custody`, { method: 'DELETE' },
	),
  createConnectorSpec: (input: ConnectorSpecInput) => request<unknown>(
    '/connector-specs',
    { method: 'POST', body: JSON.stringify(input) },
  ),
	listConnectorSpecs: () => request<ConnectorSpecRecord[]>('/connector-specs'),
	updateConnectorSpec: (connectorId: string | number, input: UpdateConnectorSpecInput) => request<ConnectorSpecRecord>(
		`/connector-specs/${connectorId}`, { method: 'PUT', body: JSON.stringify(input) },
	),
	connectorSpecDeletionImpact: (connectorId: string | number) => request<ConnectorSpecDeletionImpact>(
		`/connector-specs/${connectorId}/deletion-impact`,
	),
	deleteConnectorSpec: (connectorId: string | number, input: DeleteConnectorObjectInput) => request<{ deleted: boolean }>(
		`/connector-specs/${connectorId}`, { method: 'DELETE', body: JSON.stringify(input) },
	),
	previewListing: (input: ListingPlanRequest, signal?: AbortSignal) => request<ListingPlan>(
		'/listing-plans/preview',
		{ method: 'POST', body: JSON.stringify(input) },
		{ timeoutMs: 95_000, signal },
	),
	getListingPlan: (planId: string | number) => request<ListingPlan>(`/listing-plans/${planId}`),
	applyListing: (planId: string | number, input: ApplyListingPlanInput) => request<ListingChangeSet>(
		`/listing-plans/${planId}/apply`,
		{ method: 'POST', body: JSON.stringify(input) },
		{ timeoutMs: 120_000 },
	),
	listListingChangeSets: (planId?: string | number, limit = 20) => {
		const query = new URLSearchParams();
		if (planId) query.set('plan_id', String(planId));
		query.set('limit', String(limit));
		return request<ListingChangeSet[]>(`/listing-change-sets?${query.toString()}`);
	},
	rollbackListingChangeSet: (changeSetId: string | number) => request<ListingChangeSet>(
		`/listing-change-sets/${changeSetId}/rollback`,
		{ method: 'POST', body: JSON.stringify({ confirm_rollback: true }) },
		{ timeoutMs: 180_000 },
	),
	listMirrorPolicies: () => request<MirrorPolicy[]>('/mirror-policies'),
	createMirrorPolicy: (document: MirrorPolicyDocument) => request<MirrorPolicy>(
		'/mirror-policies', { method: 'POST', body: JSON.stringify(document) },
	),
	updateMirrorPolicy: (document: MirrorPolicyDocument) => request<MirrorPolicy>(
		`/mirror-policies/${encodeURIComponent(document.policy_uuid)}`,
		{ method: 'PUT', headers: { 'If-Match': String(document.policy_version) }, body: JSON.stringify(document) },
	),
	deleteMirrorPolicy: (policyUUID: string, policyVersion: number) => request<void>(
		`/mirror-policies/${encodeURIComponent(policyUUID)}`,
		{ method: 'DELETE', headers: { 'If-Match': String(policyVersion) } },
	),
	previewMirrorPolicy: (policyUUID: string) => request<MirrorPreview>(
		`/mirror-policies/${encodeURIComponent(policyUUID)}/preview`, { method: 'POST', body: '{}' }, { timeoutMs: 95_000 },
	),
	getMirrorPreview: (previewId: string) => request<MirrorPreview>(
		`/mirror-previews/${encodeURIComponent(previewId)}`,
	),
		applyMirrorPreview: (previewId: string, approvalGranted: boolean) => request<MirrorApplyReceipt>(
		`/mirror-previews/${encodeURIComponent(previewId)}/apply`,
		{ method: 'POST', body: JSON.stringify({ approval_granted: approvalGranted }) },
		{ timeoutMs: 180_000 },
		),
		previewMirrorAdoption: (input: MirrorAdoptionPreviewRequest) => request<MirrorAdoptionPreview>(
			'/mirror-adoptions/preview',
			{ method: 'POST', body: JSON.stringify(input) },
			{ timeoutMs: 95_000 },
		),
		getMirrorAdoptionPreview: (previewId: string) => request<MirrorAdoptionPreview>(
			`/mirror-adoption-previews/${encodeURIComponent(previewId)}`,
		),
		applyMirrorAdoption: (previewId: string, approvalGranted: boolean) => request<MirrorAdoptionReceipt>(
			`/mirror-adoption-previews/${encodeURIComponent(previewId)}/apply`,
			{ method: 'POST', body: JSON.stringify({ approval_granted: approvalGranted }) },
			{ timeoutMs: 180_000 },
		),
	listMirrorChangeSets: (filter: { policyUUID?: string; status?: string; limit?: number } = {}) => {
		const query = new URLSearchParams();
		if (filter.policyUUID) query.set('policy_uuid', filter.policyUUID);
		if (filter.status) query.set('status', filter.status);
		query.set('limit', String(filter.limit ?? 100));
		return request<MirrorChangeSet[]>(`/mirror-change-sets?${query.toString()}`);
	},
	getMirrorChangeSet: (changeSetId: string | number) => request<MirrorChangeSet>(
		`/mirror-change-sets/${changeSetId}`,
	),
	listMirrorChangeEvents: (changeSetId: string | number) => request<MirrorChangeEvent[]>(
		`/mirror-change-sets/${changeSetId}/events`,
	),
	exportPortableConfig: () => request<PortableConfigExport>('/config/export'),
	previewConfigImport: (mode: ConfigImportMode, bundle: PortableConfig) => request<ConfigImportPreview>(
		'/config/imports/preview',
		{ method: 'POST', body: JSON.stringify({ mode, bundle }) },
		{ timeoutMs: 45_000 },
	),
	applyConfigImport: (previewId: number, previewHash: string) => request<ConfigImportRun>(
		'/config/imports/apply',
		{ method: 'POST', body: JSON.stringify({ preview_id: previewId, preview_hash: previewHash }) },
		{ timeoutMs: 60_000 },
	),
	getConfigImportRun: (runId: string | number) => request<ConfigImportRun>(`/config/imports/${runId}`),
	rollbackConfigImport: (runId: string | number) => request<ConfigImportRun>(
		`/config/imports/${runId}/rollback`,
		{ method: 'POST', body: '{}' },
		{ timeoutMs: 60_000 },
	),
};
