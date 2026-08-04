importScripts('capture.js', 'custody-client.js');

const CUSTODY_ALARM = 'relayops-browser-custody';
const CHECK_INTERVAL_MINUTES = 1;
const EVENT_DEBOUNCE_MS = 2_500;
const REJECTED_CAPTURE_RETRY_MS = 60 * 1000;
const MANAGED_TAB_LOAD_TIMEOUT_MS = 20_000;
const MANAGED_TAB_LOAD_POLL_MS = 500;
const MAX_CAPTURE_TABS = 3;
const CANDIDATE_CAPTURE_TIMEOUT_MS = 15_000;
const CAPTURE_ROUND_TIMEOUT_MS = 45_000;
const CAPTURE_ATTEMPT_LIMIT = 3;

let pollPromise = null;
let lastEventPollAt = 0;

chrome.runtime.onInstalled.addListener(() => {
  void ensureAlarm();
});
chrome.runtime.onStartup.addListener(() => {
	void ensureAlarm().then(() => pollCustodyBindings(true));
});
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === CUSTODY_ALARM) void pollCustodyBindings();
});
chrome.runtime.onMessage.addListener((message) => {
	if (message?.type === 'custody-binding-updated') void ensureAlarm().then(() => pollCustodyBindings(true));
});
chrome.tabs.onUpdated.addListener((_tabId, changeInfo, tab) => {
	if (changeInfo.status === 'complete') void scheduleForTab(tab);
});
chrome.tabs.onActivated.addListener(async ({ tabId }) => {
	try { await scheduleForTab(await chrome.tabs.get(tabId)); } catch { /* tab disappeared */ }
});

async function ensureAlarm() {
  await chrome.alarms.create(CUSTODY_ALARM, { delayInMinutes: CHECK_INTERVAL_MINUTES, periodInMinutes: CHECK_INTERVAL_MINUTES });
}

async function bindings() {
  const saved = await chrome.storage.local.get(['browser_custody_bindings']);
  return saved.browser_custody_bindings && typeof saved.browser_custody_bindings === 'object'
    ? saved.browser_custody_bindings : {};
}

async function saveBindings(value) {
  await chrome.storage.local.set({ browser_custody_bindings: value });
}

function matchingOrigin(tab, expectedOrigin) {
	try { return Boolean(tab?.id) && RelayOpsCapture.httpOrigin(tab.url || '') === expectedOrigin; } catch { return false; }
}

async function waitForManagedTab(tabId, expectedOrigin, timeoutMs = MANAGED_TAB_LOAD_TIMEOUT_MS) {
	const deadline = Date.now() + Math.max(1, Math.min(MANAGED_TAB_LOAD_TIMEOUT_MS, timeoutMs));
	while (Date.now() < deadline) {
		const tab = await chrome.tabs.get(tabId);
		if (!matchingOrigin(tab, expectedOrigin)) throw new Error('助手打开的页面跳转到了未授权的其它站点');
		if (tab.status === 'complete' || !tab.status) return tab;
		await new Promise((resolve) => setTimeout(resolve, MANAGED_TAB_LOAD_POLL_MS));
	}
	throw new Error('等待上游登录页面加载超时');
}

async function captureTabs(binding) {
	const opened = (await chrome.tabs.query({ url: `${binding.expected_origin}/*` }))
		.filter((tab) => matchingOrigin(tab, binding.expected_origin))
		.sort((a, b) => (b.lastAccessed || 0) - (a.lastAccessed || 0));
	if (opened.length > 0) return opened.slice(0, MAX_CAPTURE_TABS);

	const managedID = Number(binding.managed_tab_id || 0);
	if (managedID > 0) {
		try { return [await waitForManagedTab(managedID, binding.expected_origin)]; } catch (error) {
			try { await chrome.tabs.get(managedID); } catch { binding.managed_tab_id = 0; }
			if (binding.managed_tab_id) throw error;
		}
	}
	const created = await chrome.tabs.create({ url: `${binding.expected_origin}/`, active: false });
	if (!created?.id) throw new Error('登录助手无法打开已授权的上游页面');
	binding.managed_tab_id = created.id;
	binding.managed_tab_created_at = new Date().toISOString();
	return [await waitForManagedTab(created.id, binding.expected_origin)];
}

async function closeManagedTab(binding) {
	const managedID = Number(binding.managed_tab_id || 0);
	if (managedID <= 0) return;
	binding.managed_tab_id = 0;
	binding.managed_tab_created_at = '';
	try { await chrome.tabs.remove(managedID); } catch { /* already closed */ }
}

async function captureCandidateWithinBudget(tabID, expectedOrigin, timeoutMs = CANDIDATE_CAPTURE_TIMEOUT_MS) {
	let timer;
	try {
		return await Promise.race([
			(async () => {
				const payload = await RelayOpsCapture.captureLoginState(tabID, expectedOrigin);
				const fingerprint = await RelayOpsCapture.fingerprintLoginState(payload);
				return { payload, fingerprint };
			})(),
			new Promise((_, reject) => {
				timer = setTimeout(() => reject(new Error('读取上游登录态超时；助手会在下一轮自动重试')), Math.max(1, timeoutMs));
			}),
		]);
	} finally {
		if (timer) clearTimeout(timer);
	}
}

async function captureReadyCandidate(binding, candidates) {
	const deadline = Date.now() + CAPTURE_ROUND_TIMEOUT_MS;
	const queue = candidates.slice(0, MAX_CAPTURE_TABS);
	let attempts = 0;
	let reloadUsed = false;
	let lastError = '';
	while (queue.length > 0 && attempts < CAPTURE_ATTEMPT_LIMIT && Date.now() < deadline) {
		const candidate = queue.shift();
		attempts += 1;
		try {
			const remaining = Math.max(1, deadline - Date.now());
			const captured = await captureCandidateWithinBudget(candidate.id, binding.expected_origin,
				Math.min(CANDIDATE_CAPTURE_TIMEOUT_MS, remaining));
			const rejectedAt = Date.parse(binding.last_rejected_capture_at || '');
			const sameRejected = binding.last_rejected_capture_fingerprint === captured.fingerprint && Number.isFinite(rejectedAt);
			if (!sameRejected) return captured;

			lastError = '已打开页面仍是上一次被拒绝的登录态；助手正在等待页面恢复';
			if (!reloadUsed && typeof chrome.tabs.reload === 'function' && Date.now() < deadline) {
				reloadUsed = true;
				try {
					await chrome.tabs.reload(candidate.id, { bypassCache: false });
					const reloaded = await waitForManagedTab(candidate.id, binding.expected_origin, deadline - Date.now());
					queue.unshift(reloaded);
					continue;
				} catch (error) {
					lastError = error instanceof Error ? error.message : lastError;
				}
			}
			if (Date.now() - rejectedAt >= REJECTED_CAPTURE_RETRY_MS) return captured;
		} catch (error) {
			lastError = error instanceof Error ? error.message : '上游页面读取失败';
			if (queue.length === 0 && !reloadUsed && typeof chrome.tabs.reload === 'function' && Date.now() < deadline) {
				reloadUsed = true;
				try {
					await chrome.tabs.reload(candidate.id, { bypassCache: false });
					const reloaded = await waitForManagedTab(candidate.id, binding.expected_origin, deadline - Date.now());
					queue.push(reloaded);
				} catch (reloadError) {
					lastError = reloadError instanceof Error ? reloadError.message : lastError;
				}
			}
		}
	}
	return { error: lastError || '没有可复用的上游登录态' };
}

async function pollCustodyBindings(force = false) {
	if (pollPromise) return pollPromise;
	const now = Date.now();
	if (!force && now - lastEventPollAt < EVENT_DEBOUNCE_MS) return;
	lastEventPollAt = now;
	pollPromise = pollCustodyBindingsOnce().finally(() => { pollPromise = null; });
	return pollPromise;
}

async function pollCustodyBindingsOnce() {
  const current = await bindings();
  for (const [supplierID, binding] of Object.entries(current)) {
    if (!binding?.token || !binding.relayops_origin || !binding.expected_origin) continue;
	let storageKey = supplierID;
    try {
			const statusResult = await RelayOpsCustody.request(binding.relayops_origin, 'status', binding.token);
      if (statusResult.response.status === 401 || statusResult.response.status === 404) {
        delete current[supplierID];
        continue;
      }
      if (!statusResult.response.ok) continue;
      const status = statusResult.result?.data ?? statusResult.result;
	  if (status?.supplier_id && String(status.supplier_id) !== supplierID) {
		storageKey = String(status.supplier_id);
		delete current[supplierID];
		current[storageKey] = binding;
	  }
      binding.last_check_at = new Date().toISOString();
      binding.needs_capture = Boolean(status?.needs_capture);
			binding.need_reason = status?.need_reason || '';
			binding.helper_state = status?.helper_state || '';
			if (binding.helper_state === 'upgrade_required') {
				binding.last_error = `登录助手需要升级到 ${status?.minimum_helper_version || '最新版本'}`;
				continue;
			}
      if (!binding.needs_capture) {
        binding.last_error = '';
		await closeManagedTab(binding);
        continue;
      }
	  const candidates = await captureTabs(binding);
	  let captured = false;
	  let removed = false;
	  let candidateError = '';
	  const candidate = await captureReadyCandidate(binding, candidates);
	  if (candidate.error) {
		candidateError = candidate.error;
	  } else {
		try {
		  const capture = await RelayOpsCustody.request(binding.relayops_origin, 'capture', binding.token, {
			method: 'POST', body: JSON.stringify({ payload: candidate.payload }), headers: { 'Content-Type': 'application/json' },
		  });
		  if (capture.response.status === 401 || capture.response.status === 404) {
			delete current[storageKey];
			removed = true;
		  }
		  if (capture.response.ok) {
			const result = capture.result?.data ?? capture.result;
			candidateError = result?.captured === false ? (result.reason || '') : '';
			if (result?.captured) {
			  binding.last_error = '';
			  binding.last_success_at = new Date().toISOString();
			  binding.last_rejected_capture_at = '';
			  binding.last_rejected_capture_fingerprint = '';
			  captured = true;
			  await closeManagedTab(binding);
			}
		  } else {
			const detail = capture.result?.data ?? capture.result;
			candidateError = detail?.message || `候选验证未通过（HTTP ${capture.response.status}）`;
			if (capture.response.status >= 400 && capture.response.status < 500 &&
				![408, 429].includes(capture.response.status)) {
			  binding.last_rejected_capture_at = new Date().toISOString();
			  binding.last_rejected_capture_fingerprint = candidate.fingerprint;
			}
		  }
		} catch (error) {
		  candidateError = error instanceof Error ? error.message : '上游页面读取失败';
		}
	  }
	  if (removed) continue;
	  if (!captured) binding.last_error = candidateError || '没有可复用的上游登录态';
    } catch (error) {
      binding.last_error = error instanceof Error ? error.message : '托管检查暂时失败';
    }
	current[storageKey] = binding;
  }
	await saveBindings(current);
}

async function scheduleForTab(tab) {
	if (!tab?.url) return;
	let origin;
	try { origin = RelayOpsCapture.httpOrigin(tab.url); } catch { return; }
	const current = await bindings();
	if (Object.values(current).some((binding) => binding?.expected_origin === origin)) await pollCustodyBindings(false);
}

void ensureAlarm().then(() => pollCustodyBindings(true));
