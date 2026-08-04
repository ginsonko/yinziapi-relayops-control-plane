import { useEffect, useMemo, useState, type ChangeEvent } from 'react';
import { AlertTriangle, Blocks, Braces, Check, ChevronDown, Cookie, Copy, Download, FileCheck2, FileJson, KeyRound, Link2, LogIn, Pencil, Plus, Puzzle, RefreshCw, RotateCw, ShieldCheck, ShieldOff, Target, Trash2, Upload, UserRound } from 'lucide-react';
import { api, getErrorMessage } from '../api/client';
import type { AuthCapabilityEvidence, ConfigureCredentialRenewalInput, Connector, ConnectorManifestDeletionImpact, ConnectorManifestDocument, ConnectorManifestRecord, ConnectorSpecDeletionImpact, ConnectorSpecInput, ConnectorSpecRecord, CreateCredentialInput, Credential, CredentialDeletionImpact, CredentialLoginRuleView, CredentialRenewalDetail, Supplier, SupplierManifestBinding } from '../api/types';
import { Button, IconButton, Modal, PageHeader, Section, StatusBadge, Toggle } from '../components/ui';
import { TargetSitesPanel } from './TargetSitesPanel';
import { projectSupplierCredentialBindings } from './credentialProjection';

type CredentialRow = Credential & { supplierId?: string };

const CAPABILITY_ENDPOINT_ALIASES = {
	catalog: new Set(['catalog', 'pricing', 'public_catalog', 'customer_catalog', 'customer_key_catalog']),
	balance: new Set(['balance', 'customer_balance']),
	usage: new Set(['usage', 'customer_usage']),
} satisfies Record<'catalog' | 'balance' | 'usage', ReadonlySet<string>>;

type RenewalForm = {
	enabled: boolean; username: string; password: string; url: string; method: string; contentType: string;
	usernameField: string; passwordField: string; tokenPath: string; userIdPath: string; expiresInPath: string; expiresAtPath: string;
	successPath: string; successValue: string;
	loginRefreshTokenPath: string; refreshUrl: string; refreshMethod: string; refreshTokenField: string;
	refreshResponseTokenPath: string; refreshResponseRefreshPath: string; refreshResponseExpiresPath: string; refreshResponseExpiryPath: string;
	messagePath: string; captchaPath: string; twoFactorPath: string; cookieResponse: boolean;
	minimumInterval: number; initialBackoff: number; maximumBackoff: number; maximumFailures: number;
	humanCooldown: number; allowReplaceBrowserPairing: boolean;
};

const defaultRenewalForm: RenewalForm = {
	enabled: true, username: '', password: '', url: '/api/user/login', method: 'POST', contentType: 'application/json',
	usernameField: 'username', passwordField: 'password', tokenPath: '', userIdPath: 'data.id', expiresInPath: '', expiresAtPath: '',
	successPath: 'success', successValue: '',
	loginRefreshTokenPath: '', refreshUrl: '', refreshMethod: '', refreshTokenField: '',
	refreshResponseTokenPath: '', refreshResponseRefreshPath: '', refreshResponseExpiresPath: '', refreshResponseExpiryPath: '',
	messagePath: 'message', captchaPath: '', twoFactorPath: '', cookieResponse: true,
	minimumInterval: 900, initialBackoff: 60, maximumBackoff: 3600, maximumFailures: 5,
	humanCooldown: 43200, allowReplaceBrowserPairing: false,
};

const renewalFormFromRule = (rule: CredentialLoginRuleView | undefined, enabled: boolean): RenewalForm => {
	const refreshUrl = rule?.refresh_url || '';
	return {
		...defaultRenewalForm,
		enabled,
		url: rule?.url || defaultRenewalForm.url,
		method: rule?.method || defaultRenewalForm.method,
		contentType: rule?.content_type || defaultRenewalForm.contentType,
		usernameField: rule?.username_field || defaultRenewalForm.usernameField,
		passwordField: rule?.password_field || defaultRenewalForm.passwordField,
		tokenPath: rule?.token_json_path || '',
		userIdPath: rule?.user_id_json_path || defaultRenewalForm.userIdPath,
		expiresInPath: rule?.expires_in_json_path || '',
		expiresAtPath: rule?.expires_at_json_path || '',
		successPath: rule?.success_json_path || defaultRenewalForm.successPath,
		successValue: rule?.success_value || '',
		loginRefreshTokenPath: refreshUrl ? rule?.login_refresh_token_json_path || '' : '',
		refreshUrl,
		refreshMethod: refreshUrl ? rule?.refresh_method || 'POST' : '',
		refreshTokenField: refreshUrl ? rule?.refresh_token_field || 'refresh_token' : '',
		refreshResponseTokenPath: refreshUrl ? rule?.refresh_response_token_json_path || '' : '',
		refreshResponseRefreshPath: refreshUrl ? rule?.refresh_response_refresh_token_json_path || '' : '',
		refreshResponseExpiresPath: refreshUrl ? rule?.refresh_response_expires_in_json_path || '' : '',
		refreshResponseExpiryPath: refreshUrl ? rule?.refresh_response_expires_at_json_path || '' : '',
		messagePath: rule?.message_json_path || defaultRenewalForm.messagePath,
		captchaPath: rule?.captcha_json_path || '',
		twoFactorPath: rule?.two_factor_json_path || '',
		cookieResponse: rule?.cookie_response ?? true,
		minimumInterval: rule?.minimum_interval_seconds || defaultRenewalForm.minimumInterval,
		initialBackoff: rule?.initial_backoff_seconds || defaultRenewalForm.initialBackoff,
		maximumBackoff: rule?.maximum_backoff_seconds || defaultRenewalForm.maximumBackoff,
		maximumFailures: rule?.maximum_consecutive_failures || defaultRenewalForm.maximumFailures,
		humanCooldown: rule?.human_action_cooldown_seconds || defaultRenewalForm.humanCooldown,
		allowReplaceBrowserPairing: rule?.allow_replace_browser_pairing ?? false,
	};
};


export type ConnectorIntent = { requestId: string; supplierId: string; action: 'login' | 'renewal' };

export function ConnectorsPage({ connectors, credentials, suppliers, intent, onIntentConsumed, onPair, onPairVerified, onSaveCredential, onImportConnector, onReload, adminToken, onSaveAdminToken, onNotify }: {
  connectors: Connector[];
  credentials: Credential[];
	suppliers: Supplier[];
	intent: ConnectorIntent | null;
	onIntentConsumed: (requestId: string) => void;
  onPair: (supplierId: string) => Promise<string>;
	onPairVerified: () => Promise<void>;
	onSaveCredential: (supplierId: string, input: CreateCredentialInput) => Promise<void>;
  onImportConnector: (input: ConnectorSpecInput) => Promise<void>;
	onReload: () => Promise<void>;
  adminToken: string;
  onSaveAdminToken: (token: string) => Promise<void>;
	onNotify: (message: string) => void;
}) {
  const [tab, setTab] = useState<'credentials' | 'targets' | 'connectors'>('targets');
  const [pairing, setPairing] = useState<CredentialRow | null>(null);
  const [pairCode, setPairCode] = useState('');
  const [pairLoading, setPairLoading] = useState(false);
  const [pairError, setPairError] = useState('');
	const [pairNotice, setPairNotice] = useState('');
	const [showHelperInstall, setShowHelperInstall] = useState(false);
	const [pairCopied, setPairCopied] = useState<'url' | 'code' | ''>('');
	const [pairVerified, setPairVerified] = useState(false);
	const [pairExpiresAt, setPairExpiresAt] = useState(0);
  const [importOpen, setImportOpen] = useState(false);
  const [importName, setImportName] = useState('');
  const [importVersion, setImportVersion] = useState('1');
  const [importJSON, setImportJSON] = useState('');
  const [importFileName, setImportFileName] = useState('');
  const [importError, setImportError] = useState('');
  const [importing, setImporting] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [tokenValue, setTokenValue] = useState(adminToken);
  const [tokenError, setTokenError] = useState('');
  const [tokenSaving, setTokenSaving] = useState(false);
	const [credentialOpen, setCredentialOpen] = useState(false);
	const [credentialSupplierId, setCredentialSupplierId] = useState(suppliers[0]?.id ?? '');
	const [credentialKind, setCredentialKind] = useState<'api-key' | 'site-cookie'>('api-key');
	const [credentialValue, setCredentialValue] = useState('');
	const [credentialHeader, setCredentialHeader] = useState('Authorization');
	const [credentialMode, setCredentialMode] = useState<'bearer' | 'raw' | 'header'>('bearer');
	const [credentialSaving, setCredentialSaving] = useState(false);
	const [credentialError, setCredentialError] = useState('');
	const [renewalCredential, setRenewalCredential] = useState<CredentialRow | null>(null);
	const [renewalDetail, setRenewalDetail] = useState<CredentialRenewalDetail | null>(null);
	const [renewalForm, setRenewalForm] = useState<RenewalForm>(defaultRenewalForm);
	const [renewalAdvanced, setRenewalAdvanced] = useState(false);
	const [renewalPresetLabel, setRenewalPresetLabel] = useState('');
	const [renewalLoading, setRenewalLoading] = useState(false);
	const [renewalSaving, setRenewalSaving] = useState(false);
	const [renewalError, setRenewalError] = useState('');
	const [loginCredential, setLoginCredential] = useState<CredentialRow | null>(null);
	const [loginUsername, setLoginUsername] = useState('');
	const [loginPassword, setLoginPassword] = useState('');
	const [loginRenewal, setLoginRenewal] = useState(true);
	const [loginLoading, setLoginLoading] = useState(false);
	const [loginError, setLoginError] = useState('');
	const [capabilitiesBySupplier, setCapabilitiesBySupplier] = useState<Record<string, AuthCapabilityEvidence[]>>({});
	const [capabilitiesLoading, setCapabilitiesLoading] = useState(false);
	const [manifests, setManifests] = useState<ConnectorManifestRecord[]>([]);
	const [connectorSpecs, setConnectorSpecs] = useState<ConnectorSpecRecord[]>([]);
	const [manifestSupplier, setManifestSupplier] = useState<CredentialRow | null>(null);
	const [manifestBinding, setManifestBinding] = useState<SupplierManifestBinding | null>(null);
	const [manifestSelection, setManifestSelection] = useState('');
	const [manifestLoading, setManifestLoading] = useState(false);
	const [manifestError, setManifestError] = useState('');
	const [manifestImportOpen, setManifestImportOpen] = useState(false);
	const [manifestJSON, setManifestJSON] = useState('');
	const [manifestImporting, setManifestImporting] = useState(false);
	const [manifestImportError, setManifestImportError] = useState('');
	const [connectorEditor, setConnectorEditor] = useState<ConnectorSpecRecord | null>(null);
	const [connectorEditorName, setConnectorEditorName] = useState('');
	const [connectorEditorJSON, setConnectorEditorJSON] = useState('');
	const [connectorEditorEnabled, setConnectorEditorEnabled] = useState(true);
	const [connectorEditorSaving, setConnectorEditorSaving] = useState(false);
	const [connectorEditorError, setConnectorEditorError] = useState('');
	const [manifestEditor, setManifestEditor] = useState<ConnectorManifestRecord | null>(null);
	const [manifestEditorJSON, setManifestEditorJSON] = useState('');
	const [manifestEditorSaving, setManifestEditorSaving] = useState(false);
	const [manifestEditorError, setManifestEditorError] = useState('');
	const [configurationDelete, setConfigurationDelete] = useState<{ kind: 'spec' | 'manifest'; id: number; name: string } | null>(null);
	const [configurationDeleteImpact, setConfigurationDeleteImpact] = useState<ConnectorSpecDeletionImpact | ConnectorManifestDeletionImpact | null>(null);
	const [configurationDeleteName, setConfigurationDeleteName] = useState('');
	const [configurationDeleteLoading, setConfigurationDeleteLoading] = useState(false);
	const [configurationDeleteError, setConfigurationDeleteError] = useState('');
	const [credentialDelete, setCredentialDelete] = useState<CredentialRow | null>(null);
	const [credentialDeleteImpact, setCredentialDeleteImpact] = useState<CredentialDeletionImpact | null>(null);
	const [credentialDeleteName, setCredentialDeleteName] = useState('');
	const [credentialDeleteLoading, setCredentialDeleteLoading] = useState(false);
	const [credentialDeleteError, setCredentialDeleteError] = useState('');
	const [custodyRevoke, setCustodyRevoke] = useState<CredentialRow | null>(null);
	const [custodyRevokeLoading, setCustodyRevokeLoading] = useState(false);
	const [custodyRevokeError, setCustodyRevokeError] = useState('');

  const { credentialRows, independentCredentialRows } = useMemo(() => {
    const projection = projectSupplierCredentialBindings(suppliers, credentials);
    const rows = projection.bindings.map<CredentialRow>(({ supplier, credential }) => {
      if (credential) return { ...credential, status: supplier.authStatus, supplierId: supplier.id };
      return {
        id: `missing-${supplier.id}`,
        supplier: supplier.name,
        supplierId: supplier.id,
        method: 'Unconfigured' as const,
        status: 'missing' as const,
        expiresAt: null,
        lastRefreshedAt: '从未',
        autoRefresh: false,
      };
    });
    return { credentialRows: rows, independentCredentialRows: projection.orphans };
  }, [credentials, suppliers]);

	useEffect(() => {
		let cancelled = false;
		if (tab !== 'credentials' || suppliers.length === 0) return;
		setCapabilitiesLoading(true);
		void Promise.all(suppliers.map(async (supplier) => {
			try {
				const result = await api.supplierCapabilities(supplier.id);
				return [supplier.id, result.data] as const;
			} catch {
				return [supplier.id, []] as const;
			}
		})).then((entries) => {
			if (!cancelled) setCapabilitiesBySupplier(Object.fromEntries(entries));
		}).finally(() => {
			if (!cancelled) setCapabilitiesLoading(false);
		});
		return () => { cancelled = true; };
	}, [suppliers, tab]);

	const loadConnectorConfiguration = async () => {
		const [manifestResult, connectorResult] = await Promise.all([api.listConnectorManifests(), api.listConnectorSpecs()]);
		setManifests(manifestResult.data);
		setConnectorSpecs(connectorResult.data);
	};

	useEffect(() => {
		if (tab !== 'connectors') return;
		void loadConnectorConfiguration().catch(() => { setManifests([]); setConnectorSpecs([]); });
	}, [tab]);

  const startPairing = async (credential: CredentialRow) => {
    setPairing(credential);
    setPairLoading(true);
    setPairCode('');
    setPairError('');
		setPairNotice('');
		setPairCopied('');
		setPairVerified(false);
    try {
      if (!credential.supplierId) throw new Error('该凭据尚未关联上游站点，请先在上游站点中完成关联。');
      const code = await onPair(credential.supplierId);
      setPairCode(code);
			setPairExpiresAt(Date.now() + 300_000);
    } catch (error) {
      setPairError(getErrorMessage(error));
    } finally {
      setPairLoading(false);
    }
  };

	useEffect(() => {
		if (!pairing?.supplierId || !pairCode || pairVerified) return;
		let stopped = false;
		let timer = 0;
		const poll = async () => {
			if (stopped) return;
			if (Date.now() >= pairExpiresAt) {
				setPairError('配对码已超过 5 分钟，请关闭弹窗后重新生成。');
				return;
			}
			try {
				const result = await api.pairingStatus(pairCode);
				if (result.data.status === 'completed') {
					setPairNotice('');
					setPairError('');
					setPairVerified(true);
					await onPairVerified();
					return;
				}
				if (result.data.status === 'failed') {
					setPairNotice('');
					setPairError(result.data.last_error_message || `配对失败：${result.data.last_error_class || '请重新发起配对'}`);
					return;
				}
				if (result.data.status === 'pending' && result.data.last_attempt_at) {
					if (result.data.attempt_consumed === false) {
						const retry = result.data.retry_after_seconds && result.data.retry_after_seconds > 0
							? `${result.data.retry_after_seconds} 秒后可使用同一配对码重试`
							: '现在可以使用同一配对码重试';
						setPairError('');
						setPairNotice(`${result.data.last_error_message || '上游暂时未完成只读验证'}；本次未扣配对次数，${retry}。${result.data.old_credential_preserved ? '旧凭据和旧数据均已保留。' : '本次候选登录态尚未保存。'}`);
					} else {
						setPairNotice('');
						setPairError(`${result.data.last_error_message || '上一次验证未通过'}；当前配对码还可重试 ${result.data.attempts_remaining} 次。`);
					}
				}
				if (result.data.status === 'expired') {
					setPairNotice('');
					setPairError('配对码已超过 5 分钟，请关闭弹窗后重新生成。');
					return;
				}
			} catch {
				// Keep polling; an explicit pairing error remains visible in the extension.
			}
			timer = window.setTimeout(() => void poll(), 2200);
		};
		timer = window.setTimeout(() => void poll(), 1000);
		return () => { stopped = true; window.clearTimeout(timer); };
	}, [onPairVerified, pairCode, pairExpiresAt, pairVerified, pairing?.supplierId]);

	const copyPairValue = async (kind: 'url' | 'code', value: string) => {
		try {
			let copied = false;
			if (navigator.clipboard?.writeText) {
				try {
					await navigator.clipboard.writeText(value);
					copied = true;
				} catch {
					// Tailscale HTTP pages are not secure contexts in Chromium.
				}
			}
			if (!copied) {
				const textarea = document.createElement('textarea');
				textarea.value = value;
				textarea.readOnly = true;
				textarea.style.position = 'fixed';
				textarea.style.left = '-9999px';
				textarea.style.opacity = '0';
				document.body.appendChild(textarea);
				try {
					textarea.focus();
					textarea.select();
					textarea.setSelectionRange(0, value.length);
					copied = document.execCommand('copy');
				} finally {
					textarea.remove();
					window.getSelection()?.removeAllRanges();
				}
			}
			if (!copied) throw new Error('浏览器拒绝了复制操作，请手动选择文本');
			setPairCopied(kind);
			window.setTimeout(() => setPairCopied(''), 1800);
		} catch (error) {
			setPairError(`复制失败：${getErrorMessage(error)}`);
		}
	};

  const readConnectorFile = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;
    setImportError('');
    try {
      const text = await file.text();
      const parsed = JSON.parse(text) as Record<string, unknown>;
      setImportJSON(text);
      setImportFileName(file.name);
      setImportName(typeof parsed.name === 'string' && parsed.name.trim() ? parsed.name : file.name.replace(/\.json$/i, ''));
      const version = parsed.schema_version ?? parsed.version ?? '1';
      setImportVersion(String(version));
    } catch (error) {
      setImportJSON('');
      setImportError(error instanceof SyntaxError ? '文件不是有效的 JSON，请修正后重新选择。' : getErrorMessage(error));
    }
  };

  const importConnector = async () => {
    if (!importJSON || !importName.trim() || !importVersion.trim()) return;
    setImporting(true);
    setImportError('');
    try {
      await onImportConnector({ name: importName.trim(), schema_version: importVersion.trim(), spec_json: importJSON, enabled: true });
		await loadConnectorConfiguration();
      setImportOpen(false);
      setImportJSON('');
      setImportFileName('');
      setImportName('');
      setImportVersion('1');
    } catch (error) {
      setImportError(getErrorMessage(error));
    } finally {
      setImporting(false);
    }
  };

  const saveToken = async () => {
    setTokenSaving(true);
    setTokenError('');
    try {
      await onSaveAdminToken(tokenValue);
      setTokenOpen(false);
    } catch (error) {
      setTokenError(getErrorMessage(error));
    } finally {
      setTokenSaving(false);
    }
  };

	const saveCredential = async () => {
		const supplier = suppliers.find((item) => item.id === credentialSupplierId);
		if (!supplier || !credentialValue.trim()) return;
		setCredentialSaving(true); setCredentialError('');
		try {
			const input: CreateCredentialInput = credentialKind === 'site-cookie' ? {
				name: `${supplier.name} 站点 Cookie`, kind: 'site-cookie', site_url: supplier.baseUrl,
				payload: { cookie: credentialValue.trim() },
			} : {
				name: `${supplier.name} API Key`, kind: 'api-key', site_url: supplier.baseUrl,
				payload: credentialMode === 'header'
					? { authorization: credentialValue.trim(), authorization_mode: 'header', authorization_header: credentialHeader.trim() || 'X-API-Key' }
					: { authorization: credentialValue.trim(), authorization_mode: credentialMode },
			};
			await onSaveCredential(supplier.id, input);
			setCredentialOpen(false); setCredentialValue('');
		} catch (error) { setCredentialError(getErrorMessage(error)); } finally { setCredentialSaving(false); }
	};

	const openCredentialDelete = async (credential: CredentialRow) => {
		if (credential.id.startsWith('missing-')) return;
		setCredentialDelete(credential);
		setCredentialDeleteImpact(null);
		setCredentialDeleteName('');
		setCredentialDeleteError('');
		setCredentialDeleteLoading(true);
		try {
			const result = await api.credentialDeletionImpact(credential.id);
			setCredentialDeleteImpact(result.data);
		} catch (error) {
			setCredentialDeleteError(getErrorMessage(error));
		} finally {
			setCredentialDeleteLoading(false);
		}
	};

	const confirmCredentialDelete = async () => {
		if (!credentialDelete || !credentialDeleteImpact) return;
		setCredentialDeleteLoading(true);
		setCredentialDeleteError('');
		try {
			await api.deleteCredential(credentialDelete.id, { confirm_name: credentialDeleteName, impact_hash: credentialDeleteImpact.impact_hash });
			setCredentialDelete(null);
			setCredentialDeleteImpact(null);
			setCredentialDeleteName('');
			await onPairVerified();
			onNotify('孤立凭据已删除；没有任何上游、目标站或上架计划受到影响。');
		} catch (error) {
			setCredentialDeleteError(getErrorMessage(error));
			try {
				const refreshed = await api.credentialDeletionImpact(credentialDelete.id);
				setCredentialDeleteImpact(refreshed.data);
			} catch {
				// Keep the actionable deletion error visible.
			}
		} finally {
			setCredentialDeleteLoading(false);
		}
	};

	const revokeCustody = async () => {
		if (!custodyRevoke?.supplierId) return;
		setCustodyRevokeLoading(true); setCustodyRevokeError('');
		try {
			await api.revokeBrowserCustody(custodyRevoke.supplierId);
			setCustodyRevoke(null);
			await onPairVerified();
			onNotify('浏览器托管已撤销；扩展不会再读取该上游的已打开页面。');
		} catch (error) {
			setCustodyRevokeError(getErrorMessage(error));
		} finally { setCustodyRevokeLoading(false); }
	};

	const openRenewal = async (credential: CredentialRow) => {
		if (credential.id.startsWith('missing-')) return;
		setRenewalCredential(credential); setRenewalDetail(null); setRenewalLoading(true); setRenewalError(''); setRenewalAdvanced(false); setRenewalPresetLabel('');
		try {
			const [result, presetResult] = await Promise.all([
				api.credentialRenewal(credential.id),
				credential.supplierId ? api.credentialRenewalPreset(credential.supplierId).catch(() => null) : Promise.resolve(null),
			]);
			setRenewalDetail(result.data);
			const preset = !result.data.status.configured && !result.data.rule && presetResult?.data.supported ? presetResult.data : null;
			const rule = result.data.rule ?? preset?.rule;
			setRenewalPresetLabel(preset?.label || '');
			setRenewalForm(renewalFormFromRule(rule, result.data.status.configured ? result.data.status.enabled : true));
		} catch (error) { setRenewalError(getErrorMessage(error)); } finally { setRenewalLoading(false); }
	};

	const renewalInput = (): ConfigureCredentialRenewalInput => {
		const refreshEnabled = renewalForm.refreshUrl.trim() !== '';
		return {
			enabled: renewalForm.enabled,
			...(renewalForm.username.trim() ? { username: renewalForm.username.trim() } : {}),
			...(renewalForm.password ? { password: renewalForm.password } : {}),
			rule: {
			url: renewalForm.url.trim(), method: renewalForm.method, content_type: renewalForm.contentType,
			username_field: renewalForm.usernameField.trim(), password_field: renewalForm.passwordField.trim(),
			token_json_path: renewalForm.tokenPath.trim(), user_id_json_path: renewalForm.userIdPath.trim(),
			expires_in_json_path: renewalForm.expiresInPath.trim(), expires_at_json_path: renewalForm.expiresAtPath.trim(),
			success_json_path: renewalForm.successPath.trim(), success_value: renewalForm.successValue.trim(),
			login_refresh_token_json_path: refreshEnabled ? renewalForm.loginRefreshTokenPath.trim() : '',
			refresh_url: refreshEnabled ? renewalForm.refreshUrl.trim() : '',
			refresh_method: refreshEnabled ? renewalForm.refreshMethod || 'POST' : '',
			refresh_token_field: refreshEnabled ? renewalForm.refreshTokenField.trim() || 'refresh_token' : '',
			refresh_response_token_json_path: refreshEnabled ? renewalForm.refreshResponseTokenPath.trim() : '',
			refresh_response_refresh_token_json_path: refreshEnabled ? renewalForm.refreshResponseRefreshPath.trim() : '',
			refresh_response_expires_in_json_path: refreshEnabled ? renewalForm.refreshResponseExpiresPath.trim() : '',
			refresh_response_expires_at_json_path: refreshEnabled ? renewalForm.refreshResponseExpiryPath.trim() : '',
			message_json_path: renewalForm.messagePath.trim(), captcha_json_path: renewalForm.captchaPath.trim(),
			two_factor_json_path: renewalForm.twoFactorPath.trim(), cookie_response: renewalForm.cookieResponse,
			minimum_interval_seconds: renewalForm.minimumInterval, initial_backoff_seconds: renewalForm.initialBackoff,
			maximum_backoff_seconds: renewalForm.maximumBackoff, maximum_consecutive_failures: renewalForm.maximumFailures,
			human_action_cooldown_seconds: renewalForm.humanCooldown,
			allow_replace_browser_pairing: renewalForm.allowReplaceBrowserPairing,
		},
		};
	};

	const updateRefreshUrl = (value: string) => setRenewalForm((current) => {
		if (value.trim() === '') {
			return { ...current, refreshUrl: value, refreshMethod: '', refreshTokenField: '', loginRefreshTokenPath: '',
				refreshResponseTokenPath: '', refreshResponseRefreshPath: '', refreshResponseExpiresPath: '', refreshResponseExpiryPath: '' };
		}
		return { ...current, refreshUrl: value, refreshMethod: current.refreshMethod || 'POST',
			refreshTokenField: current.refreshTokenField || 'refresh_token' };
	});

	const saveRenewal = async () => {
		if (!renewalCredential) return;
		if ((!renewalDetail?.username_configured && !renewalForm.username.trim()) || (!renewalDetail?.password_configured && !renewalForm.password)) {
			setRenewalError('首次配置需要填写上游登录账号和密码。'); return;
		}
		setRenewalSaving(true); setRenewalError('');
		try {
			const result = await api.configureCredentialRenewal(renewalCredential.id, renewalInput());
			setRenewalDetail(result.data); setRenewalForm((current) => ({ ...current, username: '', password: '' }));
			await onPairVerified(); onNotify(result.data.status.enabled ? '自动续约已启用，账号密码仅保存在服务端加密凭据中。' : '自动续约配置已保存并暂停。');
		} catch (error) { setRenewalError(getErrorMessage(error)); } finally { setRenewalSaving(false); }
	};

	const triggerRenewal = async () => {
		if (!renewalCredential) return;
		setRenewalSaving(true); setRenewalError('');
		try { const result = await api.triggerCredentialRenewal(renewalCredential.id); setRenewalDetail(result.data); await onPairVerified(); onNotify('已完成一次登录续约与只读能力验证。'); }
		catch (error) { setRenewalError(getErrorMessage(error)); } finally { setRenewalSaving(false); }
	};

	const removeRenewal = async () => {
		if (!renewalCredential) return;
		setRenewalSaving(true); setRenewalError('');
		try { const result = await api.removeCredentialRenewal(renewalCredential.id); setRenewalDetail(result.data); setRenewalForm(defaultRenewalForm); await onPairVerified(); onNotify('自动续约配置和加密登录信息已移除。'); }
		catch (error) { setRenewalError(getErrorMessage(error)); } finally { setRenewalSaving(false); }
	};

	const openInitialLogin = (credential: CredentialRow) => {
		setLoginCredential(credential);
		setLoginUsername('');
		setLoginPassword('');
		setLoginRenewal(true);
		setLoginError('');
	};

	useEffect(() => {
		if (!intent) return;
		setTab('credentials');
		const credential = credentialRows.find((item) => item.supplierId === intent.supplierId);
		if (!credential) {
			onNotify('没有找到对应上游的凭据入口，请刷新后重试。');
			onIntentConsumed(intent.requestId);
			return;
		}
		if (intent.action === 'login' || credential.id.startsWith('missing-')) {
			openInitialLogin(credential);
		} else {
			void openRenewal(credential);
		}
		onIntentConsumed(intent.requestId);
	}, [intent?.requestId]);

	const submitInitialLogin = async () => {
		if (!loginCredential?.supplierId || !loginUsername.trim() || !loginPassword) return;
		setLoginLoading(true);
		setLoginError('');
		try {
			await api.loginSupplier(loginCredential.supplierId, {
				username: loginUsername.trim(), password: loginPassword, enable_renewal: loginRenewal,
			});
			setLoginCredential(null);
			setLoginUsername('');
			setLoginPassword('');
			await onPairVerified();
			onNotify('账号登录、只读能力验证和站点绑定已完成，首次采集已在后台启动。');
		} catch (error) {
			setLoginError(getErrorMessage(error));
		} finally {
			setLoginLoading(false);
		}
	};

	const capabilityState = (supplierId: string | undefined, endpoint: 'catalog' | 'balance' | 'usage') => {
		const evidence = supplierId ? capabilitiesBySupplier[supplierId] ?? [] : [];
		const samples = evidence.filter((item) => CAPABILITY_ENDPOINT_ALIASES[endpoint].has(item.endpoint_type ?? ''));
		if (samples.some((item) => item.status === 'verified')) return 'verified';
		if (samples.some((item) => item.status === 'expired_or_invalid')) return 'expired';
		if (samples.some((item) => item.status === 'contract_changed')) return 'contract';
		if (samples.some((item) => item.status && item.status !== 'unknown')) return 'unavailable';
		return 'unknown';
	};

	const capabilityLabel = (state: string) => ({
		verified: '已验证', expired: '登录过期', contract: '契约变化', unavailable: '暂不可达', unknown: '尚未验证',
	}[state] || state);

	const openManifestBinding = async (credential: CredentialRow) => {
		if (!credential.supplierId) return;
		setManifestSupplier(credential);
		setManifestLoading(true);
		setManifestError('');
		setManifestBinding(null);
		setManifestSelection('');
		try {
			const manifestResult = await api.listConnectorManifests();
			setManifests(manifestResult.data);
			try {
				const bindingResult = await api.getSupplierConnectorManifest(credential.supplierId);
				setManifestBinding(bindingResult.data);
				setManifestSelection(String(bindingResult.data.manifest_id));
			} catch {
				setManifestSelection('');
			}
		} catch (error) {
			setManifestError(getErrorMessage(error));
		} finally {
			setManifestLoading(false);
		}
	};

	const saveManifestBinding = async () => {
		if (!manifestSupplier?.supplierId) return;
		setManifestLoading(true);
		setManifestError('');
		try {
			await api.bindSupplierConnectorManifest(manifestSupplier.supplierId, manifestSelection ? Number(manifestSelection) : null);
			setManifestSupplier(null);
			onNotify(manifestSelection ? '站点 Manifest 已绑定，后续采集将使用声明式端点与认证契约。' : '已恢复使用内置站点契约。');
		} catch (error) {
			setManifestError(getErrorMessage(error));
		} finally {
			setManifestLoading(false);
		}
	};

	const importManifest = async () => {
		setManifestImporting(true);
		setManifestImportError('');
		try {
			const parsed = JSON.parse(manifestJSON) as ConnectorManifestDocument;
			await api.importConnectorManifest(parsed);
			await loadConnectorConfiguration();
			setManifestImportOpen(false);
			setManifestJSON('');
			onNotify('声明式站点 Manifest 已校验并导入。');
		} catch (error) {
			setManifestImportError(error instanceof SyntaxError ? 'Manifest 不是有效 JSON。' : getErrorMessage(error));
		} finally {
			setManifestImporting(false);
		}
	};

	const openConnectorEditor = (record: ConnectorSpecRecord) => {
		setConnectorEditor(record);
		setConnectorEditorName(record.name);
		setConnectorEditorJSON(record.spec_json);
		setConnectorEditorEnabled(record.enabled);
		setConnectorEditorError('');
	};

	const saveConnectorEditor = async () => {
		if (!connectorEditor || !connectorEditorName.trim() || !connectorEditorJSON.trim()) return;
		setConnectorEditorSaving(true);
		setConnectorEditorError('');
		try {
			JSON.parse(connectorEditorJSON);
			await api.updateConnectorSpec(connectorEditor.id, {
				name: connectorEditorName.trim(), schema_version: connectorEditor.schema_version,
				spec_json: connectorEditorJSON, enabled: connectorEditorEnabled, expected_updated_at: connectorEditor.updated_at,
			});
			setConnectorEditor(null);
			await onReload();
			await loadConnectorConfiguration();
			onNotify('JSON Connector 已更新；后续采集立即使用新版本。');
		} catch (error) {
			setConnectorEditorError(error instanceof SyntaxError ? 'Connector 内容不是有效 JSON。' : getErrorMessage(error));
		} finally {
			setConnectorEditorSaving(false);
		}
	};

	const openManifestEditor = (record: ConnectorManifestRecord) => {
		setManifestEditor(record);
		setManifestEditorJSON(JSON.stringify(record.manifest, null, 2));
		setManifestEditorError('');
	};

	const saveManifestEditor = async () => {
		if (!manifestEditor || !manifestEditorJSON.trim()) return;
		setManifestEditorSaving(true);
		setManifestEditorError('');
		try {
			const parsed = JSON.parse(manifestEditorJSON) as ConnectorManifestDocument;
			await api.updateConnectorManifest(manifestEditor.id, parsed, manifestEditor.updated_at);
			setManifestEditor(null);
			await loadConnectorConfiguration();
			onNotify('站点 Manifest 已校验并更新，已绑定站点无需重新绑定。');
		} catch (error) {
			setManifestEditorError(error instanceof SyntaxError ? 'Manifest 不是有效 JSON。' : getErrorMessage(error));
		} finally {
			setManifestEditorSaving(false);
		}
	};

	const openConfigurationDelete = async (kind: 'spec' | 'manifest', id: number, name: string) => {
		setConfigurationDelete({ kind, id, name });
		setConfigurationDeleteImpact(null);
		setConfigurationDeleteName('');
		setConfigurationDeleteError('');
		setConfigurationDeleteLoading(true);
		try {
			const result = kind === 'spec' ? await api.connectorSpecDeletionImpact(id) : await api.connectorManifestDeletionImpact(id);
			setConfigurationDeleteImpact(result.data);
			setConfigurationDelete({ kind, id, name: result.data.name });
		} catch (error) {
			setConfigurationDeleteError(getErrorMessage(error));
		} finally {
			setConfigurationDeleteLoading(false);
		}
	};

	const confirmConfigurationDelete = async () => {
		if (!configurationDelete || !configurationDeleteImpact) return;
		setConfigurationDeleteLoading(true);
		setConfigurationDeleteError('');
		try {
			const input = { confirm_name: configurationDeleteImpact.name, impact_hash: configurationDeleteImpact.impact_hash };
			if (configurationDelete.kind === 'spec') await api.deleteConnectorSpec(configurationDelete.id, input);
			else await api.deleteConnectorManifest(configurationDelete.id, input);
			setConfigurationDelete(null);
			await onReload();
			await loadConnectorConfiguration();
			onNotify(configurationDelete.kind === 'spec' ? '自定义 Connector 已删除。' : '站点 Manifest 已删除。');
		} catch (error) {
			setConfigurationDeleteError(getErrorMessage(error));
		} finally {
			setConfigurationDeleteLoading(false);
		}
	};

	const downloadManifest = (record: ConnectorManifestRecord) => {
		const url = URL.createObjectURL(new Blob([JSON.stringify(record.manifest, null, 2)], { type: 'application/json' }));
		const link = document.createElement('a');
		link.href = url;
		link.download = `${record.name.replace(/[^A-Za-z0-9_.-]+/g, '-') || 'connector'}-manifest.json`;
		link.click();
		URL.revokeObjectURL(url);
	};

	const downloadConnectorGuide = () => {
		const guide = {
			schema_version: 1,
			name: 'example-pricing-connector',
			limits: { timeout_seconds: 25, max_response_bytes: 2097152 },
			endpoints: [
				{
					name: 'catalog', method: 'GET', path: '/api/catalog',
					groups: { items_path: 'data.groups', external_id: { path: 'id', required: true }, name: { path: 'name', required: true }, ratio: { path: 'ratio' }, model_ids: { path: 'models' } },
					models: { items_path: 'data.models', external_id: { path: 'id', required: true }, name: { path: 'name', required: true }, group: { path: 'group' }, billing_mode: { path: 'billing_mode' }, model_ratio: { path: 'model_ratio' }, completion_ratio: { path: 'completion_ratio' }, currency: { path: 'currency' } },
				},
				{
					name: 'balance', method: 'GET', path: '/api/user/self', headers: { Authorization: { credential_ref: 'authorization' } },
					balance: { root_path: 'data', amount: { path: 'balance', required: true }, available: { path: 'available' }, currency: { path: 'currency' }, daily_spend: { path: 'daily_spend' } },
				},
			],
		};
		const url = URL.createObjectURL(new Blob([JSON.stringify(guide, null, 2)], { type: 'application/json' }));
		const link = document.createElement('a'); link.href = url; link.download = 'connector-rule-guide.json'; link.click(); URL.revokeObjectURL(url);
	};
	const credentialMethodLabel = (method: Credential['method']) => ({
		'Browser Pairing': '浏览器登录配对', 'API Key': 'API Key', 'Access Token': 'Access Token', Cookie: '站点 Cookie', Unconfigured: '尚未授权',
	})[method];
	const renewalStateLabel = (state?: string) => ({
		healthy: '自动续约正常', disabled: '自动续约已暂停', unconfigured: '未配置自动续约', auth_expired: '等待续约',
		password_login_attempt: '正在登录续约', refresh_token_attempt: '正在刷新', retry_backoff: '等待重试',
		suspended_backoff: '失败退避中', human_action_required: '需要人工验证', permission_denied: '权限不足', configuration_required: '配置待完善',
	}[state || 'unconfigured'] || state || '未配置自动续约');
	const renderCredentialRenewalState = (credential: CredentialRow) => {
		const custody = credential.browserCustody;
		if (custody?.enabled) {
			const recoveryPending = custody.needs_capture && custody.recovery_pending && !custody.recovery_overdue;
			const warning = custody.helper_state !== 'online' || custody.recovery_overdue || (custody.needs_capture && !recoveryPending);
			const label = custody.helper_state === 'upgrade_required' ? '登录助手需要升级'
				: custody.helper_state === 'offline' ? '登录助手当前离线'
					: custody.helper_state === 'binding_pending' ? '等待登录助手确认'
						: recoveryPending ? '正在自动续约'
							: custody.needs_capture ? '浏览器托管等待接管' : '浏览器托管在线';
			const detail = custody.helper_state === 'upgrade_required' ? `请安装 v${custody.minimum_helper_version ?? '1.3.0'} 或更新版本`
				: custody.helper_state === 'offline' ? '扩展超过 6 分钟没有心跳；请确认浏览器和扩展正在运行'
					: custody.helper_state === 'binding_pending' ? '安装新版扩展并完成本次配对后自动确认'
						: recoveryPending ? '无需操作；扩展正在到期前刷新，成功后会自动更新有效期'
							: custody.needs_capture ? '自动续约未在预期时间内完成；请保持该上游已登录页面打开'
							: custody.last_success_at ? `v${custody.helper_version ?? '?'} · 上次接管 ${new Date(custody.last_success_at).toLocaleString('zh-CN', { hour12: false })}`
								: `v${custody.helper_version ?? '?'} · 仅在核心授权明确失效时读取页面`;
			return <div className="credential-renewal-state"><span className={warning ? 'warning' : 'positive'}>{label}</span><small>{detail}</small></div>;
		}
		if (credential.recollectionPending) {
			return <div className="credential-renewal-state"><span className="positive">凭据已恢复，正在复采</span><small>无需操作，完成后自动更新</small></div>;
		}
		if (credential.status === 'valid' && credential.renewal?.human_action) {
			return <div className="credential-renewal-state"><span className="positive">当前授权有效</span><small>核心只读能力已验证；以后失效时可启用浏览器托管，当前无需处理</small></div>;
		}
		const detail = credential.renewal?.next_attempt_at ? `下次 ${new Date(credential.renewal.next_attempt_at).toLocaleString('zh-CN', { hour12: false })}`
			: credential.renewal?.last_success_at ? `上次成功 ${new Date(credential.renewal.last_success_at).toLocaleString('zh-CN', { hour12: false })}` : '可按需配置';
		return <div className="credential-renewal-state"><span className={credential.renewal?.human_action ? 'warning' : credential.autoRefresh ? 'positive' : ''}>{renewalStateLabel(credential.renewal?.state)}</span><small>{detail}</small></div>;
	};
	const isSub2Credential = (credential: CredentialRow | null) => suppliers.find((supplier) => supplier.id === credential?.supplierId)?.kind === 'Sub2API';
	const configurationDeleteRefs = configurationDeleteImpact?.supplier_refs ?? [];
	const configurationDeleteBlocked = configurationDeleteRefs.length > 0;
	const credentialDeleteSupplierRefs = credentialDeleteImpact?.supplier_refs ?? [];
	const credentialDeleteTargetRefs = credentialDeleteImpact?.target_refs ?? [];
	const credentialDeleteBlocked = credentialDeleteSupplierRefs.length > 0 || credentialDeleteTargetRefs.length > 0 || (credentialDeleteImpact?.listing_plan_refs ?? 0) > 0;

  return (
    <>
      <PageHeader eyebrow="接入能力" title="连接器、目标站与凭据" description="上游登录、本站 NewAPI 管理凭据和采集连接器分开维护；敏感值只进入服务端加密存储。" actions={<>{tab === 'credentials' && <Button onClick={() => setCredentialOpen(true)}><Plus size={17} />添加 API Key / Cookie</Button>}{tab === 'connectors' && <Button onClick={() => setImportOpen(true)}><Upload size={17} />导入 JSON Connector</Button>}<Button variant="secondary" onClick={() => { setTokenValue(adminToken); setTokenError(''); setTokenOpen(true); }}><KeyRound size={17} />控制台令牌</Button></>} />
      <div className="page-tabs"><button className={tab === 'credentials' ? 'active' : ''} onClick={() => setTab('credentials')}><KeyRound size={17} />上游凭据</button><button className={tab === 'targets' ? 'active' : ''} onClick={() => setTab('targets')}><Target size={17} />本站 NewAPI</button><button className={tab === 'connectors' ? 'active' : ''} onClick={() => setTab('connectors')}><Blocks size={17} />连接器</button></div>

      <Modal open={!!loginCredential} onClose={() => !loginLoading && setLoginCredential(null)} title={`登录 ${loginCredential?.supplier ?? ''}`} description="优先使用站点的声明式登录契约完成首次连接。登录成功后会先验证目录、余额和用量等只读能力，再原子保存并绑定凭据。" footer={<><Button variant="ghost" onClick={() => setLoginCredential(null)} disabled={loginLoading}>取消</Button><Button variant="secondary" onClick={() => { const current = loginCredential; setLoginCredential(null); if (current) void startPairing(current); }} disabled={loginLoading}><Puzzle size={16} />验证码 / 2FA / 浏览器登录</Button><Button onClick={() => void submitInitialLogin()} disabled={loginLoading || !loginUsername.trim() || !loginPassword}>{loginLoading && <RefreshCw size={16} className="spin" />}{loginLoading ? '正在登录并验证' : '登录并连接'}</Button></>}>
		<div className="login-connect-form">
			<div className="form-grid">
				<label className="field"><span>{isSub2Credential(loginCredential) ? '上游登录邮箱' : '上游账号'}</span><input autoComplete="username" value={loginUsername} onChange={(event) => setLoginUsername(event.target.value)} placeholder={isSub2Credential(loginCredential) ? '必须填写登录邮箱，不是用户名' : '用户名或邮箱'} /></label>
				<label className="field"><span>上游密码</span><input type="password" autoComplete="current-password" value={loginPassword} onChange={(event) => setLoginPassword(event.target.value)} placeholder="仅加密保存到服务端凭据" /></label>
			</div>
			<label className="check-row"><input type="checkbox" checked={loginRenewal} onChange={(event) => setLoginRenewal(event.target.checked)} /><span>登录失效后自动续约<small>仅在凭据过期或只读能力返回明确的登录失效证据时触发；验证码和 2FA 会转为人工接管。</small></span></label>
			{loginError && <div className="form-error">{loginError}</div>}
			<div className="info-callout"><ShieldCheck size={18} /><div><strong>验证通过才会保存</strong><span>账号密码、Cookie 和 Token 不会回显到浏览器，也不会进入活动日志。</span></div></div>
		</div>
      </Modal>

	  <Modal open={!!manifestSupplier} onClose={() => !manifestLoading && setManifestSupplier(null)} title={`${manifestSupplier?.supplier ?? ''} · 站点契约`} description="内置契约适合标准 NewAPI / Sub2API；站点接口有差异时，可绑定经过严格校验的 Manifest。" footer={<><Button variant="ghost" onClick={() => setManifestSupplier(null)} disabled={manifestLoading}>取消</Button><Button onClick={() => void saveManifestBinding()} disabled={manifestLoading}>{manifestLoading && <RefreshCw size={16} className="spin" />}保存绑定</Button></>}>
		<div className="manifest-binding-form">
			<label className="field"><span>采集与登录契约</span><select value={manifestSelection} onChange={(event) => setManifestSelection(event.target.value)}><option value="">使用内置契约</option>{manifests.map((manifest) => <option key={manifest.id} value={manifest.id}>{manifest.name} · {manifest.adapter_family}</option>)}</select></label>
			{manifestBinding?.manifest && <div className="manifest-current"><FileCheck2 size={18} /><div><strong>当前：{manifestBinding.manifest.name}</strong><small>{manifestBinding.bound_origin} · 绑定于 {new Date(manifestBinding.bound_at).toLocaleString('zh-CN', { hour12: false })}</small></div></div>}
			{manifestError && <div className="form-error">{manifestError}</div>}
			<div className="info-callout"><FileJson size={18} /><div><strong>绑定时再次检查来源站点</strong><span>Manifest 只能声明同源相对路径、请求头语义和 JSON 字段路径，无法携带静态密钥或执行脚本。</span></div></div>
		</div>
	  </Modal>

	  <Modal open={manifestImportOpen} onClose={() => !manifestImporting && setManifestImportOpen(false)} title="导入站点 Manifest" description="粘贴版本化 JSON。后端会拒绝未知字段、跨域 URL、脚本、模板和静态秘密。" footer={<><Button variant="ghost" onClick={() => setManifestImportOpen(false)} disabled={manifestImporting}>取消</Button><Button onClick={() => void importManifest()} disabled={manifestImporting || !manifestJSON.trim()}>{manifestImporting && <RefreshCw size={16} className="spin" />}{manifestImporting ? '正在校验' : '校验并导入'}</Button></>}>
		<label className="field"><span>Manifest JSON</span><textarea rows={15} value={manifestJSON} onChange={(event) => setManifestJSON(event.target.value)} placeholder={'{\n  "schema_version": "relayops.connector-manifest/v1",\n  "name": "example-newapi",\n  "adapter_family": "newapi"\n}'} /></label>
		{manifestImportError && <div className="form-error">{manifestImportError}</div>}
		<div className="connector-checks"><span><Check size={15} />同源路径</span><span><Check size={15} />严格版本</span><span><Check size={15} />无脚本与静态秘密</span></div>
	  </Modal>

      {tab === 'credentials' ? <>
        <div className="credential-health">
          <div><span className="credential-shield"><ShieldCheck size={24} /></span><div><strong>凭据在本机加密保存</strong><p>浏览器配对只读取当前站点会话；可选的自动续约账号密码使用同一主密钥加密，列表和日志均不回显。</p></div></div>
		  <span>{credentialRows.filter((item) => item.status === 'valid').length} / {credentialRows.length} 凭据状态有效</span>
        </div>
		<div className="credential-kind-guide">
			<div><Link2 size={18} /><strong>浏览器配对与失效托管</strong><span>首次由你配对；登录助手在线后，仅在核心授权明确失效时，从已打开的同源页面自动接管新会话。</span></div>
			<div><KeyRound size={18} /><strong>API Key / Access Token</strong><span>适合上游开放业务或管理 API，可选择 Bearer、原值或自定义请求头。</span></div>
			<div><RotateCw size={18} /><strong>账号密码自动续约</strong><span>普通登录失效时自动低频续约；验证码或二步验证会暂停并提示人工接管。</span></div>
		</div>
        <Section title="站点授权" description="每个上游都会出现在这里，没有凭据的站点也可以直接发起配对">
          <div className="credential-list">
            {[...credentialRows].sort((a, b) => (a.status === 'valid' ? 1 : 0) - (b.status === 'valid' ? 1 : 0)).map((credential) => (
              <article className="credential-row" key={credential.id}>
				<span className={`credential-method status-${credential.status === 'valid' ? 'healthy' : credential.status === 'expiring' || credential.status === 'pending' ? 'warning' : 'error'}`}><KeyRound size={18} /></span>
                <div className="credential-name"><strong>{credential.supplier}</strong><span>{credentialMethodLabel(credential.method)}</span></div>
				<div className="credential-capability-summary" aria-label="只读能力状态">
					{(['catalog', 'balance', 'usage'] as const).map((capability) => {
						const state = capabilityState(credential.supplierId, capability);
						return <span key={capability} className={`capability-state capability-${state}`} title={state === 'unknown' ? '最近尚无该能力的验证样本' : undefined}>
							{capability === 'catalog' ? '目录' : capability === 'balance' ? '余额' : '用量'} · {capabilityLabel(state)}
							{capability === 'balance' && suppliers.find((item) => item.id === credential.supplierId)?.balance !== null && suppliers.find((item) => item.id === credential.supplierId)?.balance !== undefined
								? ` · ${state === 'verified' ? '' : '旧快照 '}${suppliers.find((item) => item.id === credential.supplierId)?.balance}` : ''}
						</span>;
					})}
					{capabilitiesLoading && <RefreshCw size={14} className="spin" />}
					{credential.status === 'missing' && <Button variant="primary" onClick={() => openInitialLogin(credential)} disabled={!credential.supplierId}><LogIn size={16} />账号密码登录</Button>}
					<Button variant="ghost" onClick={() => void openManifestBinding(credential)} disabled={!credential.supplierId}><FileCheck2 size={16} />站点契约</Button>
				</div>
                <StatusBadge status={credential.status} />
                <div className="credential-time"><span>有效期</span><strong>{credential.expiresAt ?? '长期有效'}</strong></div>
                <div className="credential-time"><span>最近刷新</span><strong>{credential.lastRefreshedAt}</strong></div>
				{renderCredentialRenewalState(credential)}
				<div className="credential-actions"><Button variant="secondary" onClick={() => void openRenewal(credential)} disabled={credential.id.startsWith('missing-')}><UserRound size={16} />自动续约</Button><Button variant={credential.status === 'valid' ? 'secondary' : 'primary'} onClick={() => void startPairing(credential)} disabled={!credential.supplierId}><Link2 size={16} />{credential.status === 'missing' ? '开始配对' : '重新授权'}</Button>{credential.browserCustody?.enabled && credential.supplierId && <IconButton label={`关闭 ${credential.supplier} 的浏览器托管`} onClick={() => { setCustodyRevokeError(''); setCustodyRevoke(credential); }}><ShieldOff size={16} /></IconButton>}{!credential.id.startsWith('missing-') && <IconButton className="icon-button-danger" label={`删除 ${credential.supplier} 的凭据`} onClick={() => void openCredentialDelete(credential)}><Trash2 size={16} /></IconButton>}</div>
              </article>
            ))}
          </div>
        </Section>
        {independentCredentialRows.length > 0 && <Section title="独立渠道密钥" description="供一键上架或指定渠道调用；不属于站点登录，因此无需浏览器重新授权">
          <div className="credential-list">
            {independentCredentialRows.map((credential) => (
              <article className="credential-row" key={credential.id}>
                <span className={`credential-method status-${credential.status === 'valid' ? 'healthy' : credential.status === 'expiring' || credential.status === 'pending' ? 'warning' : 'error'}`}><KeyRound size={18} /></span>
                <div className="credential-name"><strong>{credential.supplier}</strong><span>{credentialMethodLabel(credential.method)}</span></div>
                <div className="credential-capability-summary"><span>渠道专用</span><small>不用于站点登录和浏览器托管</small></div>
                <StatusBadge status={credential.status} />
                <div className="credential-time"><span>有效期</span><strong>{credential.expiresAt ?? '长期有效'}</strong></div>
                <div className="credential-time"><span>最近刷新</span><strong>{credential.lastRefreshedAt}</strong></div>
                <div className="credential-renewal-state"><span>独立密钥</span><small>在一键上架或渠道配置中引用</small></div>
                <div className="credential-actions"><IconButton className="icon-button-danger" label={`删除 ${credential.supplier} 的独立密钥`} onClick={() => void openCredentialDelete(credential)}><Trash2 size={16} /></IconButton></div>
              </article>
            ))}
          </div>
        </Section>}
      </> : tab === 'targets' ? <TargetSitesPanel onChanged={onNotify} /> : <>
		<div className="manifest-toolbar">
			<div><FileCheck2 size={20} /><span><strong>版本化站点 Manifest</strong><small>为 NewAPI / Sub2API 声明端点、认证头与登录契约；仅允许同源相对路径，不执行脚本。</small></span></div>
			<Button variant="secondary" onClick={() => { setManifestImportError(''); setManifestImportOpen(true); }}><Upload size={16} />导入 Manifest</Button>
		</div>
		{manifests.length > 0 && <div className="manifest-list">{manifests.map((manifest) => <article key={manifest.id}>
			<span className="manifest-icon"><FileCheck2 size={18} /></span><div><strong>{manifest.name}</strong><small>{manifest.adapter_family} · {manifest.schema_version}</small></div>
			<StatusBadge status={manifest.validation_status === 'valid' ? 'healthy' : 'error'} label={manifest.validation_status === 'valid' ? '已校验' : '不可用'} />
			<div className="manifest-actions"><IconButton label={`导出 ${manifest.name}`} onClick={() => downloadManifest(manifest)}><Download size={16} /></IconButton><IconButton label={`编辑 ${manifest.name}`} onClick={() => openManifestEditor(manifest)}><Pencil size={16} /></IconButton><IconButton className="icon-button-danger" label={`删除 ${manifest.name}`} onClick={() => void openConfigurationDelete('manifest', manifest.id, manifest.name)}><Trash2 size={16} /></IconButton></div>
		</article>)}</div>}
        <div className="connector-grid">
          {connectors.map((connector) => {
			const spec = connector.kind === 'json' ? connectorSpecs.find((item) => String(item.id) === connector.id) : undefined;
			return <article className="connector-card" key={connector.id}>
              <div className="connector-head"><span className={connector.kind === 'builtin' ? 'builtin' : 'custom'}>{connector.kind === 'builtin' ? <Blocks size={20} /> : <Braces size={20} />}</span><StatusBadge status={connector.status} label={spec && !spec.enabled ? '已暂停' : connector.supplierCount === 0 ? '尚未验证' : undefined} /></div>
              <strong>{connector.name}</strong><p>{connector.protocol} · {connector.version}</p>
              <div className="capabilities">{connector.capabilities.map((item) => <span key={item}>{item}</span>)}</div>
			  <div className="connector-foot"><span>{connector.supplierCount} 个站点使用</span>{spec ? <div className="connector-card-actions"><IconButton label={`编辑 ${connector.name}`} onClick={() => openConnectorEditor(spec)}><Pencil size={16} /></IconButton><IconButton className="icon-button-danger" label={`删除 ${connector.name}`} onClick={() => void openConfigurationDelete('spec', spec.id, spec.name)}><Trash2 size={16} /></IconButton></div> : <small>内置连接器</small>}</div>
			</article>;
		  })}
          <button className="connector-add" onClick={() => setImportOpen(true)}><Plus size={23} /><strong>导入自定义连接器</strong><span>通过受限 JSON 规则适配新的价格页</span></button>
        </div>
        <div className="connector-guidance"><FileJson size={21} /><div><strong>让 Codex 帮你生成连接器规则</strong><p>下载规则说明并交给大模型分析目标站点，再把生成的 JSON 导回这里。规则只能读取指定页面和字段，不能执行脚本。</p></div><Button variant="secondary" onClick={downloadConnectorGuide}><Download size={16} />下载规则说明</Button></div>
      </>}

	  <Modal open={!!connectorEditor} onClose={() => !connectorEditorSaving && setConnectorEditor(null)} title={`编辑 ${connectorEditor?.name ?? ''}`} description="保存前后端都会严格校验声明式规则；版本已经变化时会拒绝覆盖并要求重新打开。" footer={<><Button variant="ghost" onClick={() => setConnectorEditor(null)} disabled={connectorEditorSaving}>取消</Button><Button onClick={() => void saveConnectorEditor()} disabled={connectorEditorSaving || !connectorEditorName.trim() || !connectorEditorJSON.trim()}>{connectorEditorSaving && <RefreshCw size={16} className="spin" />}{connectorEditorSaving ? '正在校验' : '保存修改'}</Button></>}>
		<div className="connector-editor">
			<div className="form-grid"><label className="field"><span>显示名称</span><input value={connectorEditorName} onChange={(event) => setConnectorEditorName(event.target.value)} /></label><label className="switch-field"><span><strong>启用连接器</strong><small>暂停后保留配置，但不会用于新采集。</small></span><Toggle checked={connectorEditorEnabled} onChange={setConnectorEditorEnabled} label="启用自定义 Connector" /></label></div>
			<label className="field"><span>Connector JSON</span><textarea rows={17} value={connectorEditorJSON} onChange={(event) => setConnectorEditorJSON(event.target.value)} spellCheck={false} /></label>
			{connectorEditorError && <div className="form-error">{connectorEditorError}</div>}
			<div className="info-callout"><ShieldCheck size={18} /><div><strong>并发修改不会被覆盖</strong><span>保存使用页面打开时的版本号；若其他窗口已经更新，系统会要求重新读取后再保存。</span></div></div>
		</div>
	  </Modal>

	  <Modal open={!!manifestEditor} onClose={() => !manifestEditorSaving && setManifestEditor(null)} title={`编辑 ${manifestEditor?.name ?? ''}`} description="完整替换版本化站点契约；已绑定站点继续引用同一记录，下一次采集使用新内容。" footer={<><Button variant="ghost" onClick={() => setManifestEditor(null)} disabled={manifestEditorSaving}>取消</Button><Button onClick={() => void saveManifestEditor()} disabled={manifestEditorSaving || !manifestEditorJSON.trim()}>{manifestEditorSaving && <RefreshCw size={16} className="spin" />}{manifestEditorSaving ? '正在校验' : '保存 Manifest'}</Button></>}>
		<div className="connector-editor">
			<label className="field"><span>Manifest JSON</span><textarea rows={18} value={manifestEditorJSON} onChange={(event) => setManifestEditorJSON(event.target.value)} spellCheck={false} /></label>
			{manifestEditorError && <div className="form-error">{manifestEditorError}</div>}
			<div className="info-callout"><FileCheck2 size={18} /><div><strong>保存前重新校验全部契约</strong><span>未知字段、跨域地址、脚本、静态秘密和不支持的能力都会被拒绝。</span></div></div>
		</div>
	  </Modal>

	  <Modal open={!!configurationDelete} onClose={() => !configurationDeleteLoading && setConfigurationDelete(null)} title={`删除 ${configurationDelete?.name ?? ''}`} description={configurationDelete?.kind === 'spec' ? '自定义 Connector 被上游使用时不能删除；请先在上游配置中迁移连接器。' : 'Manifest 被站点绑定时不能删除；请先恢复内置契约或绑定其它 Manifest。'} footer={<><Button variant="ghost" onClick={() => setConfigurationDelete(null)} disabled={configurationDeleteLoading}>取消</Button><Button variant="danger" onClick={() => void confirmConfigurationDelete()} disabled={configurationDeleteLoading || !configurationDeleteImpact || configurationDeleteBlocked || configurationDeleteName.trim() !== configurationDeleteImpact.name}>{configurationDeleteLoading && <RefreshCw size={16} className="spin" />}{configurationDeleteLoading ? '正在核对' : '确认删除'}</Button></>}>
		{configurationDeleteLoading && !configurationDeleteImpact ? <div className="inline-loading"><RefreshCw size={18} className="spin" />核对全部上游引用…</div> : configurationDeleteImpact && <div className="deletion-impact">
			<div className="deletion-impact-grid"><div><span>上游引用</span><strong>{configurationDeleteRefs.length}</strong><small>{configurationDeleteRefs.map((item) => item.name).join('、') || '无'}</small></div><div><span>处理结果</span><strong>{configurationDeleteBlocked ? '禁止删除' : '可以删除'}</strong><small>{configurationDeleteBlocked ? '先迁移或解除引用' : '不影响任何采集任务'}</small></div></div>
			{configurationDeleteBlocked ? <div className="form-error"><AlertTriangle size={16} />仍有上游依赖此配置，系统已阻止删除，避免无人值守采集中断。</div> : <div className="danger-callout"><Trash2 size={18} /><div><strong>当前没有任何运行引用</strong><span>删除只影响 RelayOps 中的这条配置，不会向上游站点或本站 NewAPI 发送删除请求。</span></div></div>}
			<label className="field"><span>输入完整名称以确认</span><input autoComplete="off" value={configurationDeleteName} onChange={(event) => setConfigurationDeleteName(event.target.value)} placeholder={configurationDeleteImpact.name} /><small>必须精确输入：{configurationDeleteImpact.name}</small></label>
		</div>}
		{configurationDeleteError && <div className="form-error">{configurationDeleteError}</div>}
	  </Modal>

	  <Modal open={!!credentialDelete} onClose={() => !credentialDeleteLoading && setCredentialDelete(null)} title={`删除凭据 ${credentialDeleteImpact?.credential_name ?? ''}`} description="只有没有任何站点或上架计划引用的孤立凭据才能删除。密钥内容不会显示在影响预览或审计日志中。" footer={<><Button variant="ghost" onClick={() => setCredentialDelete(null)} disabled={credentialDeleteLoading}>取消</Button><Button variant="danger" onClick={() => void confirmCredentialDelete()} disabled={credentialDeleteLoading || !credentialDeleteImpact || credentialDeleteName.trim() !== credentialDeleteImpact.credential_name || credentialDeleteBlocked}>{credentialDeleteLoading && <RefreshCw size={16} className="spin" />}{credentialDeleteLoading ? '正在核对' : '确认删除凭据'}</Button></>}>
		{credentialDeleteLoading && !credentialDeleteImpact ? <div className="inline-loading"><RefreshCw size={18} className="spin" />核对站点和上架计划引用…</div> : credentialDeleteImpact && <div className="deletion-impact">
			<div className="deletion-impact-grid"><div><span>上游引用</span><strong>{credentialDeleteSupplierRefs.length}</strong><small>{credentialDeleteSupplierRefs.map((item) => item.name).join('、') || '无'}</small></div><div><span>目标站引用</span><strong>{credentialDeleteTargetRefs.length}</strong><small>{credentialDeleteTargetRefs.map((item) => item.name).join('、') || '无'}</small></div><div><span>上架计划引用</span><strong>{credentialDeleteImpact.listing_plan_refs}</strong><small>历史或待执行计划</small></div><div><span>自动续约配置</span><strong>{credentialDeleteImpact.renewal_configured ? '有' : '无'}</strong><small>删除凭据时一并移除</small></div></div>
			{credentialDeleteBlocked ? <div className="form-error">该凭据仍在使用中。请先重新授权对应站点，或删除不再需要的配置；系统不会级联断开站点。</div> : <div className="danger-callout"><Trash2 size={18} /><div><strong>凭据当前没有任何引用</strong><span>确认后会删除加密载荷和自动续约配置，操作不可恢复。</span></div></div>}
			<label className="field"><span>输入完整凭据名称以确认</span><input autoComplete="off" value={credentialDeleteName} onChange={(event) => setCredentialDeleteName(event.target.value)} placeholder={credentialDeleteImpact.credential_name} /><small>必须精确输入：{credentialDeleteImpact.credential_name}</small></label>
		</div>}
		{credentialDeleteError && <div className="form-error">{credentialDeleteError}</div>}
	  </Modal>

	  <Modal open={!!renewalCredential} onClose={() => !renewalSaving && setRenewalCredential(null)} title={`${renewalCredential?.supplier ?? ''} · 自动续约`} description="保存上游登录账号和密码后，仅在会话失效、即将过期或你手动触发时登录；验证码和二步验证会自动转入人工接管。" footer={<><Button variant="ghost" onClick={() => setRenewalCredential(null)} disabled={renewalSaving}>关闭</Button>{renewalDetail?.status.configured && <Button variant="ghost" onClick={() => void removeRenewal()} disabled={renewalSaving}><Trash2 size={16} />移除配置</Button>}<Button onClick={() => void saveRenewal()} disabled={renewalSaving || renewalLoading}>{renewalSaving && <RefreshCw size={16} className="spin" />}{renewalSaving ? '正在加密保存' : '保存自动续约'}</Button></>}>
		{renewalLoading ? <div className="inline-loading"><RefreshCw size={18} className="spin" />读取续约状态…</div> : <div className="renewal-editor">
			{renewalDetail && <div className={`renewal-status renewal-${renewalDetail.status.state}`}><RotateCw size={18} /><div><strong>{renewalStateLabel(renewalDetail.status.state)}</strong><span>{renewalDetail.status.human_action ? `需要处理：${renewalDetail.status.human_action}` : renewalDetail.status.last_error_class ? `最近错误：${renewalDetail.status.last_error_class}` : renewalDetail.status.enabled ? '会话失效时由服务端自动续约并先完成只读能力验证。' : '配置已保存，目前不会自动登录。'}</span></div>{renewalDetail.status.configured && renewalDetail.status.enabled && <Button variant="secondary" onClick={() => void triggerRenewal()} disabled={renewalSaving}><RefreshCw size={15} />立即验证续约</Button>}</div>}
			<label className="check-row"><input type="checkbox" checked={renewalForm.enabled} onChange={(event) => setRenewalForm({ ...renewalForm, enabled: event.target.checked })} /><span>启用登录失效自动续约<small>关闭后保留加密配置，但不会自动发起登录。</small></span></label>
			<div className="form-grid renewal-basic">
				<label className="field"><span>{isSub2Credential(renewalCredential) ? '上游登录邮箱' : '上游登录账号'}</span><input autoComplete="off" value={renewalForm.username} onChange={(event) => setRenewalForm({ ...renewalForm, username: event.target.value })} placeholder={renewalDetail?.username_configured ? '已保存；留空保持不变' : isSub2Credential(renewalCredential) ? '必须填写登录邮箱，不是用户名' : '用户名或邮箱'} /></label>
				<label className="field"><span>上游登录密码</span><input type="password" autoComplete="new-password" value={renewalForm.password} onChange={(event) => setRenewalForm({ ...renewalForm, password: event.target.value })} placeholder={renewalDetail?.password_configured ? '已加密保存；留空保持不变' : '输入登录密码'} /></label>
			</div>
			<button className="renewal-advanced-toggle" type="button" onClick={() => setRenewalAdvanced((value) => !value)}><ChevronDown size={17} className={renewalAdvanced ? 'expanded' : ''} /><span><strong>高级登录映射</strong><small>{renewalPresetLabel ? `已按${renewalPresetLabel}预填，仍可完整编辑` : '未识别的站点可在这里填写真实登录契约'}</small></span></button>
			{renewalAdvanced && <div className="form-grid renewal-advanced">
				<label className="field full"><span>登录路径</span><input value={renewalForm.url} onChange={(event) => setRenewalForm({ ...renewalForm, url: event.target.value })} /></label>
				<label className="field"><span>请求方法</span><select value={renewalForm.method} onChange={(event) => setRenewalForm({ ...renewalForm, method: event.target.value })}><option>POST</option><option>PUT</option></select></label>
				<label className="field"><span>内容类型</span><select value={renewalForm.contentType} onChange={(event) => setRenewalForm({ ...renewalForm, contentType: event.target.value })}><option value="application/json">JSON</option><option value="application/x-www-form-urlencoded">表单</option></select></label>
				<label className="field"><span>账号字段</span><input value={renewalForm.usernameField} onChange={(event) => setRenewalForm({ ...renewalForm, usernameField: event.target.value })} /></label>
				<label className="field"><span>密码字段</span><input value={renewalForm.passwordField} onChange={(event) => setRenewalForm({ ...renewalForm, passwordField: event.target.value })} /></label>
				<label className="field"><span>Token 路径（可空）</span><input value={renewalForm.tokenPath} onChange={(event) => setRenewalForm({ ...renewalForm, tokenPath: event.target.value })} /></label>
				<label className="field"><span>用户 ID 路径</span><input value={renewalForm.userIdPath} onChange={(event) => setRenewalForm({ ...renewalForm, userIdPath: event.target.value })} /></label>
				<label className="field"><span>有效期秒数路径</span><input value={renewalForm.expiresInPath} onChange={(event) => setRenewalForm({ ...renewalForm, expiresInPath: event.target.value })} placeholder="例如 data.expires_in" /></label>
				<label className="field"><span>绝对到期时间路径</span><input value={renewalForm.expiresAtPath} onChange={(event) => setRenewalForm({ ...renewalForm, expiresAtPath: event.target.value })} /></label>
				<label className="field"><span>成功状态路径</span><input value={renewalForm.successPath} onChange={(event) => setRenewalForm({ ...renewalForm, successPath: event.target.value })} /></label>
				<label className="field"><span>成功值（可空）</span><input value={renewalForm.successValue} onChange={(event) => setRenewalForm({ ...renewalForm, successValue: event.target.value })} placeholder="例如 Sub2API 的 0" /></label>
				<label className="field"><span>错误消息路径</span><input value={renewalForm.messagePath} onChange={(event) => setRenewalForm({ ...renewalForm, messagePath: event.target.value })} /></label>
				<label className="field"><span>验证码状态路径</span><input value={renewalForm.captchaPath} onChange={(event) => setRenewalForm({ ...renewalForm, captchaPath: event.target.value })} /></label>
				<label className="field"><span>二步验证状态路径</span><input value={renewalForm.twoFactorPath} onChange={(event) => setRenewalForm({ ...renewalForm, twoFactorPath: event.target.value })} /></label>
				<label className="field full"><span>Token 刷新路径（可选）</span><input value={renewalForm.refreshUrl} onChange={(event) => updateRefreshUrl(event.target.value)} placeholder="留空则在会话失效时使用账号密码重新登录" /><small>仅当上游明确提供 Refresh Token 接口时填写，例如 /api/v1/auth/refresh。</small></label>
				<label className="field"><span>登录返回 Refresh Token 路径</span><input disabled={!renewalForm.refreshUrl.trim()} value={renewalForm.loginRefreshTokenPath} onChange={(event) => setRenewalForm({ ...renewalForm, loginRefreshTokenPath: event.target.value })} /></label>
				<label className="field"><span>刷新请求方法</span><select disabled={!renewalForm.refreshUrl.trim()} value={renewalForm.refreshMethod} onChange={(event) => setRenewalForm({ ...renewalForm, refreshMethod: event.target.value })}><option value="">未启用</option><option value="POST">POST</option></select></label>
				<label className="field"><span>刷新令牌字段</span><input disabled={!renewalForm.refreshUrl.trim()} value={renewalForm.refreshTokenField} onChange={(event) => setRenewalForm({ ...renewalForm, refreshTokenField: event.target.value })} /></label>
				<label className="field"><span>刷新返回 Token 路径</span><input disabled={!renewalForm.refreshUrl.trim()} value={renewalForm.refreshResponseTokenPath} onChange={(event) => setRenewalForm({ ...renewalForm, refreshResponseTokenPath: event.target.value })} /></label>
				<label className="field"><span>刷新返回新 Refresh Token 路径</span><input disabled={!renewalForm.refreshUrl.trim()} value={renewalForm.refreshResponseRefreshPath} onChange={(event) => setRenewalForm({ ...renewalForm, refreshResponseRefreshPath: event.target.value })} /></label>
				<label className="field"><span>刷新有效期秒数路径</span><input disabled={!renewalForm.refreshUrl.trim()} value={renewalForm.refreshResponseExpiresPath} onChange={(event) => setRenewalForm({ ...renewalForm, refreshResponseExpiresPath: event.target.value })} /></label>
				<label className="field"><span>刷新绝对到期时间路径</span><input disabled={!renewalForm.refreshUrl.trim()} value={renewalForm.refreshResponseExpiryPath} onChange={(event) => setRenewalForm({ ...renewalForm, refreshResponseExpiryPath: event.target.value })} /></label>
				<label className="field"><span>最小续约间隔（秒）</span><input type="number" min="10" value={renewalForm.minimumInterval} onChange={(event) => setRenewalForm({ ...renewalForm, minimumInterval: Number(event.target.value) })} /></label>
				<label className="field"><span>首次失败退避（秒）</span><input type="number" min="1" value={renewalForm.initialBackoff} onChange={(event) => setRenewalForm({ ...renewalForm, initialBackoff: Number(event.target.value) })} /></label>
				<label className="field"><span>失败退避上限（秒）</span><input type="number" min="60" value={renewalForm.maximumBackoff} onChange={(event) => setRenewalForm({ ...renewalForm, maximumBackoff: Number(event.target.value) })} /></label>
				<label className="field"><span>连续失败暂停阈值</span><input type="number" min="1" max="20" value={renewalForm.maximumFailures} onChange={(event) => setRenewalForm({ ...renewalForm, maximumFailures: Number(event.target.value) })} /></label>
				<label className="field"><span>人工接管冷却（秒）</span><input type="number" min="60" value={renewalForm.humanCooldown} onChange={(event) => setRenewalForm({ ...renewalForm, humanCooldown: Number(event.target.value) })} /></label>
				<label className="check-row full"><input type="checkbox" checked={renewalForm.cookieResponse} onChange={(event) => setRenewalForm({ ...renewalForm, cookieResponse: event.target.checked })} /><span>从响应 Cookie 建立新会话</span></label>
				{renewalCredential?.method === 'Browser Pairing' && <label className="check-row full"><input type="checkbox" checked={renewalForm.allowReplaceBrowserPairing} onChange={(event) => setRenewalForm({ ...renewalForm, allowReplaceBrowserPairing: event.target.checked })} /><span>允许自动登录替换浏览器配对会话<small>只在新会话通过目录、余额等只读能力验证后原子替换。</small></span></label>}
			</div>}
			{renewalError && <div className="form-error">{renewalError}</div>}
			<div className="info-callout"><ShieldCheck size={18} /><div><strong>不会自动处理验证码或二步验证</strong><span>遇到人机验证会停止重试并提示人工重新授权，避免锁号和请求风暴；旧凭据与可信数据继续保留。</span></div></div>
		</div>}
	  </Modal>

	  <Modal open={!!custodyRevoke} onClose={() => !custodyRevokeLoading && setCustodyRevoke(null)} title={`关闭 ${custodyRevoke?.supplier ?? ''} 的浏览器托管`} description="关闭后保留当前凭据和历史数据，但扩展不再从已打开页面自动恢复该站点授权。" footer={<><Button variant="ghost" onClick={() => setCustodyRevoke(null)} disabled={custodyRevokeLoading}>取消</Button><Button variant="danger" onClick={() => void revokeCustody()} disabled={custodyRevokeLoading}>{custodyRevokeLoading && <RefreshCw size={16} className="spin" />}确认关闭托管</Button></>}>
		<div className="info-callout"><ShieldOff size={18} /><div><strong>不会删除当前登录凭据</strong><span>以后仍可通过“重新授权”再次建立受限托管；撤销会立即使扩展保存的旧令牌失效。</span></div></div>
		{custodyRevokeError && <div className="form-error">{custodyRevokeError}</div>}
	  </Modal>

	  <Modal open={!!pairing} onClose={() => !pairLoading && setPairing(null)} title={`配对 ${pairing?.supplier ?? ''}`} description="登录助手是本项目提供的 Chrome / Edge 浏览器扩展。首次配对后可托管该站点：只有核心授权明确失效时才读取已打开的同源页面。" footer={<><Button variant="ghost" onClick={() => setPairing(null)} disabled={pairLoading}>稍后处理</Button><Button onClick={() => setPairing(null)} disabled={!pairVerified}>{pairVerified ? <Check size={16} /> : <RefreshCw size={16} className="spin" />}{pairVerified ? '配对完成' : '正在等待扩展提交'}</Button></>}>
        <div className="pair-flow">
		  <button className="pair-helper-toggle" onClick={() => setShowHelperInstall((value) => !value)}><Puzzle size={17} /><span><strong>{showHelperInstall ? '收起扩展安装步骤' : '首次使用：安装浏览器登录助手'}</strong><small>只需安装一次，之后所有上游都可复用</small></span></button>
		  {showHelperInstall && <div className="pair-install-panel"><a className="button button-secondary pair-helper-download" href="/downloads/relayops-login-helper.zip" download><Download size={15} />下载登录助手 v1.3.8</a><div><b>1</b><span>下载并解压 <code>relayops-login-helper.zip</code>。</span></div><div><b>2</b><span>Chrome 打开 <code>chrome://extensions</code>，Edge 打开 <code>edge://extensions</code>。</span></div><div><b>3</b><span>开启“开发者模式”，点击“加载已解压的扩展程序”，选择解压后的 <code>browser-extension</code> 文件夹；更新旧版后点击一次“重新加载”。</span></div><small>扩展不会填写密码或绕过验证码；健康时只向 RelayOps 发送心跳，只有后端确认核心授权失效时，才在已授权的同源页面内有界读取登录状态。同一份失效会话不会重复提交，登录态变化后立即重试。</small></div>}
          <div className="pair-step active"><span>1</span><div><strong>先在同一个浏览器登录上游</strong><small>打开 {pairing?.supplier} 的网页，正常登录并保持该标签页为当前页面</small></div></div>
          <div className={`pair-step ${pairCode ? 'active' : ''}`}><span>2</span><div><strong>打开扩展，填写控制台地址和一次性码</strong><small>控制台地址与配对码都可以一键复制</small></div></div>
		  <div className="pair-value-row"><span>控制台地址</span><code>{window.location.origin}</code><IconButton label="复制控制台地址" onClick={() => void copyPairValue('url', window.location.origin)}>{pairCopied === 'url' ? <Check size={17} /> : <Copy size={17} />}</IconButton></div>
		  <div className="pair-value-row pair-code-row"><span>一次性配对码</span><code>{pairLoading ? '正在生成…' : pairCode || '--------'}</code><IconButton label="复制一次性配对码" onClick={() => void copyPairValue('code', pairCode)} disabled={!pairCode}>{pairCopied === 'code' ? <Check size={17} /> : <Copy size={17} />}</IconButton></div>
		  <div className="pair-expiry"><ShieldCheck size={16} /><span>配对码 5 分钟内只能建立一次凭据；成功后转为仅绑定该上游的托管令牌，不能访问控制台或修改价格。</span></div>
		  {pairError && <div className="form-error">{pairError}</div>}
		  {pairNotice && <div className="info-callout"><RefreshCw size={18} /><div><strong>会话已提交，正在等待上游恢复</strong><span>{pairNotice}</span></div></div>}
		  <div className={`pair-step ${pairVerified ? 'done' : ''}`}><span>{pairVerified ? <Check size={16} /> : '3'}</span><div><strong>{pairVerified ? '控制台已确认配对与托管' : '回到本页等待自动验证'}</strong><small>{pairVerified ? '凭据已关联并启动首次采集；有效会话不会被重复探测或替换' : '无需刷新页面；扩展提交后，本页会在几秒内自动更新状态'}</small></div></div>
        </div>
      </Modal>

		<Modal open={credentialOpen} onClose={() => !credentialSaving && setCredentialOpen(false)} title="添加站点凭据" description="这里用于 API Key 或已知 Cookie；需要从已登录网页自动读取会话时，请使用站点列表中的“开始配对”。" footer={<><Button variant="ghost" onClick={() => setCredentialOpen(false)} disabled={credentialSaving}>取消</Button><Button onClick={() => void saveCredential()} disabled={credentialSaving || !credentialSupplierId || !credentialValue.trim()}>{credentialSaving && <RefreshCw size={16} className="spin" />}{credentialSaving ? '保存并验证采集' : '保存并验证采集'}</Button></>}>
			<div className="segmented credential-type-tabs"><button className={credentialKind === 'api-key' ? 'active' : ''} onClick={() => setCredentialKind('api-key')}><KeyRound size={15} />API Key / Token</button><button className={credentialKind === 'site-cookie' ? 'active' : ''} onClick={() => setCredentialKind('site-cookie')}><Cookie size={15} />站点 Cookie</button></div>
			<div className="form-grid credential-form">
				<label className="field full"><span>关联上游</span><select value={credentialSupplierId} onChange={(event) => setCredentialSupplierId(event.target.value)}>{suppliers.map((supplier) => <option value={supplier.id} key={supplier.id}>{supplier.name}</option>)}</select></label>
				{credentialKind === 'api-key' && <><label className="field"><span>发送方式</span><select value={credentialMode} onChange={(event) => setCredentialMode(event.target.value as typeof credentialMode)}><option value="bearer">Authorization: Bearer</option><option value="raw">Authorization 原值</option><option value="header">自定义请求头</option></select></label>{credentialMode === 'header' && <label className="field"><span>请求头名称</span><input value={credentialHeader} onChange={(event) => setCredentialHeader(event.target.value)} placeholder="X-API-Key" /></label>}</>}
				<label className="field full"><span>{credentialKind === 'api-key' ? 'API Key / Token' : 'Cookie 请求头值'}</span><input type="password" autoComplete="off" value={credentialValue} onChange={(event) => setCredentialValue(event.target.value)} placeholder={credentialKind === 'api-key' ? '输入上游提供的密钥' : '例如 session=...; token=...'} /></label>
			</div>
			{credentialError && <div className="form-error">{credentialError}</div>}
			<div className="info-callout"><ShieldCheck size={18} /><div><strong>保存后立即验证</strong><span>系统会关联凭据并重新采集该站点；401 或 500 会原样显示，并保留上次可信快照。</span></div></div>
		</Modal>

      <Modal open={importOpen} onClose={() => !importing && setImportOpen(false)} title="导入 JSON Connector" description="文件会原样写入受限 Connector 规范，后端会再次校验 JSON 与安全约束。" footer={<><Button variant="ghost" onClick={() => setImportOpen(false)} disabled={importing}>取消</Button><Button onClick={() => void importConnector()} disabled={importing || !importJSON || !importName.trim()}>{importing && <RefreshCw size={16} className="spin" />}{importing ? '正在导入' : '导入并启用'}</Button></>}>
        <label className="upload-zone"><input type="file" accept="application/json,.json" onChange={(event) => void readConnectorFile(event)} /><FileJson size={28} /><strong>{importFileName || '选择或拖入 JSON 规则文件'}</strong><span>{importJSON ? '文件读取完成，可核对名称和版本' : '仅接受符合 Connector Schema 的声明式规则'}</span></label>
        {importJSON && <div className="form-grid import-meta"><label className="field"><span>连接器名称</span><input value={importName} onChange={(event) => setImportName(event.target.value)} /></label><label className="field"><span>Schema 版本</span><input value={importVersion} onChange={(event) => setImportVersion(event.target.value)} /></label></div>}
        {importError && <div className="form-error">{importError}</div>}
        <div className="connector-checks"><span><Check size={15} />仅允许 HTTPS 目标</span><span><Check size={15} />禁止执行任意脚本</span><span><Check size={15} />敏感字段自动加密</span></div>
      </Modal>

      <Modal open={tokenOpen} onClose={() => !tokenSaving && setTokenOpen(false)} title="管理接口令牌" description="令牌仅保存在当前浏览器，并通过 X-RelayOps-Token 请求头发送给本控制台。" footer={<><Button variant="ghost" onClick={() => setTokenOpen(false)} disabled={tokenSaving}>取消</Button><Button onClick={() => void saveToken()} disabled={tokenSaving}>{tokenSaving && <RefreshCw size={16} className="spin" />}{tokenSaving ? '正在验证' : '保存并验证'}</Button></>}>
        <label className="field"><span>RelayOps Admin Token</span><input type="password" autoComplete="off" value={tokenValue} onChange={(event) => setTokenValue(event.target.value)} placeholder="输入管理令牌；留空可清除" /></label>
        {tokenError && <div className="form-error">{tokenError}</div>}
        <div className="info-callout"><ShieldCheck size={18} /><div><strong>仅用于控制面</strong><span>该令牌不会发送给任何上游站点，也不会作为模型 API Key 使用。</span></div></div>
      </Modal>
    </>
  );
}
