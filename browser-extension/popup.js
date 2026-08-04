const relayopsInput = document.querySelector('#relayops');
const codeInput = document.querySelector('#code');
const statusOutput = document.querySelector('#status');
const pairButton = document.querySelector('#pair');
const siteLabel = document.querySelector('#site');
const custodyButton = document.querySelector('#custody');

let activeTab;
const requestTimeoutMs = 7000;
const pairingSubmitTimeoutMs = 45000;

initialize();

async function initialize() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  activeTab = tab;
  try {
    siteLabel.textContent = tab?.url ? httpOrigin(tab.url) : '无法读取当前站点';
  } catch {
    siteLabel.textContent = '请打开已登录的上游网页';
  }
  const saved = await chrome.storage.local.get(['relayops_url', 'browser_custody_bindings']);
  if (saved.relayops_url) relayopsInput.value = saved.relayops_url;
  const savedBindings = saved.browser_custody_bindings || {};
  if (tab?.url) {
    try {
      const origin = httpOrigin(tab.url);
      if (savedBindings[origin] || Object.values(savedBindings).some((item) => item?.expected_origin === origin)) {
        custodyButton.hidden = false;
      }
    } catch { /* non-HTTP tabs are not eligible */ }
  }
}

pairButton.addEventListener('click', async () => {
  setStatus('正在检查 RelayOps 地址…', 'working');
  pairButton.disabled = true;
  const originalLabel = pairButton.textContent;
  pairButton.textContent = '安全检查中…';
  try {
    const relayops = parseRelayOpsURL(relayopsInput.value);
    const upstream = currentUpstream();
    const code = codeInput.value.trim();
    if (!code) throw new Error('请输入控制台生成的一次性配对码');

    await requestOrigins([relayops.origin, upstream.origin]);
    await verifyRelayOps(relayops.origin);
    setStatus('RelayOps 已确认，正在核对一次性配对码…', 'working');
    const pairing = await pendingPairing(relayops.origin, code);
    if (pairing.expectedOrigin !== upstream.origin) {
      throw new Error(`当前站点不匹配：当前页面是 ${upstream.origin}，配对码绑定的是 ${pairing.expectedOrigin}。请打开正确的上游页面；配对码尚未使用。`);
    }

    setStatus('站点匹配，正在读取当前域的登录态…', 'working');
    activeTab = await requireActiveOrigin(upstream.origin);
    const payload = await captureLoginState(activeTab.id, upstream.origin);

    setStatus('登录态已读取并提交，RelayOps 正在进行上游只读验证…', 'working');
    let response;
    let result;
    let completedAfterDisconnect = false;
    try {
      ({ response, result } = await submitPairing(relayops.origin, code, upstream, payload));
    } catch (submitError) {
      completedAfterDisconnect = await recoverPairingSubmission(relayops.origin, code, submitError);
    }
    if (!completedAfterDisconnect && !response.ok) throw new Error(pairingError(result, response.status));
    await chrome.storage.local.set({ relayops_url: relayops.origin });
    const custodyReady = await enableBrowserCustody(relayops.origin, code, upstream.origin);
    setStatus(custodyReady
      ? '配对成功，浏览器托管已启用；只有上游明确失效时才会接管并复采。'
      : '配对成功，控制台已经开始首次采集；托管状态将在下一次检查时确认。', 'success');
  } catch (error) {
    setStatus(error instanceof Error ? error.message : '配对失败', 'error');
  } finally {
    pairButton.disabled = false;
    pairButton.textContent = originalLabel;
  }
});

custodyButton?.addEventListener('click', async () => {
  custodyButton.disabled = true;
  try {
    const origin = currentUpstream().origin;
    const saved = await chrome.storage.local.get(['browser_custody_bindings']);
    const all = saved.browser_custody_bindings || {};
    const key = Object.keys(all).find((supplierID) => all[supplierID]?.expected_origin === origin);
    const binding = key ? all[key] : null;
    if (!binding) throw new Error('当前站点没有浏览器托管绑定');
    const result = await custodyRequest(binding.relayops_origin, 'revoke', binding.token, { method: 'DELETE' });
    if (!result.response.ok) throw new Error('服务端没有确认撤销');
    delete all[key];
    await chrome.storage.local.set({ browser_custody_bindings: all });
    custodyButton.hidden = true;
    setStatus('当前站点的浏览器托管已撤销；扩展不会再读取该页面。', 'success');
  } catch (error) {
    setStatus(error instanceof Error ? error.message : '撤销托管失败', 'error');
  } finally {
    custodyButton.disabled = false;
  }
});

async function enableBrowserCustody(relayopsOrigin, token, expectedOrigin) {
	const pendingKey = `pending:${expectedOrigin}`;
	const provisional = {
		token, relayops_origin: RelayOpsCustody.relayOpsOrigin(relayopsOrigin), expected_origin: expectedOrigin,
		needs_capture: false, need_reason: '', helper_state: 'binding_pending',
	};
	const saved = await chrome.storage.local.get(['browser_custody_bindings']);
	const pendingBindings = saved.browser_custody_bindings || {};
	pendingBindings[pendingKey] = provisional;
	await chrome.storage.local.set({ browser_custody_bindings: pendingBindings });
  try {
    const result = await custodyRequest(relayopsOrigin, 'status', token);
	if (!result.response.ok) {
		if (result.response.status === 401 || result.response.status === 404) {
			delete pendingBindings[pendingKey];
			await chrome.storage.local.set({ browser_custody_bindings: pendingBindings });
		}
		return false;
	}
    const status = result.result?.data ?? result.result;
	if (status?.expected_origin !== expectedOrigin || !status?.enabled || !status?.supplier_id) {
		delete pendingBindings[pendingKey];
		await chrome.storage.local.set({ browser_custody_bindings: pendingBindings });
		return false;
	}
	const latest = await chrome.storage.local.get(['browser_custody_bindings']);
	const all = latest.browser_custody_bindings || {};
	delete all[pendingKey];
    all[String(status.supplier_id)] = {
      token, relayops_origin: relayopsOrigin, expected_origin: expectedOrigin,
	  expires_at: status.expires_at, needs_capture: Boolean(status.needs_capture), need_reason: status.need_reason || '',
	  helper_state: status.helper_state || 'online', last_check_at: new Date().toISOString(),
    };
    await chrome.storage.local.set({ browser_custody_bindings: all });
    return true;
  } catch {
    return false;
	} finally {
		const latest = await chrome.storage.local.get(['browser_custody_bindings']);
		const retained = Object.values(latest.browser_custody_bindings || {}).some((item) => item?.expected_origin === expectedOrigin);
		custodyButton.hidden = !retained;
		if (retained && chrome.runtime?.sendMessage) chrome.runtime.sendMessage({ type: 'custody-binding-updated' }).catch(() => {});
  }
}

async function custodyRequest(origin, route, token, options = {}) {
	return RelayOpsCustody.request(origin, route, token, { ...options, timeoutMs: pairingSubmitTimeoutMs });
}

async function captureLoginState(tabId, upstreamOrigin) {
	return RelayOpsCapture.captureLoginState(tabId, upstreamOrigin);
}

function parseRelayOpsURL(raw) {
  try {
	return new URL(RelayOpsCustody.relayOpsOrigin(raw));
  } catch (error) {
	if (error instanceof Error && /必须使用 HTTPS/.test(error.message)) throw error;
    throw new Error('RelayOps 地址格式无效，请填写完整地址，例如 http://127.0.0.1:18180');
  }
}

function httpOrigin(raw) {
  const parsed = new URL(raw);
  if (!['http:', 'https:'].includes(parsed.protocol)) throw new Error('不是 HTTP 页面');
  return parsed.origin;
}

function currentUpstream() {
  if (!activeTab?.id || !activeTab.url) throw new Error('请在已登录的上游网页中使用配对助手');
  return new URL(httpOrigin(activeTab.url));
}

async function requireActiveOrigin(expectedOrigin) {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab?.id || !tab.url) throw new Error('无法读取当前标签页，请重新打开上游网页');
  let actualOrigin;
  try {
    actualOrigin = httpOrigin(tab.url);
  } catch {
    throw new Error('当前标签页不是可配对的 HTTP 上游网页');
  }
  if (actualOrigin !== expectedOrigin) {
    throw new Error(`当前标签页已切换到 ${actualOrigin}，为避免读取错误站点，本次配对已停止。`);
  }
  return tab;
}

async function verifyRelayOps(relayopsOrigin) {
  let response;
  let result;
  try {
    ({ response, result } = await fetchJSON(`${relayopsOrigin}/api/v1/health`));
  } catch (error) {
    const suffix = error instanceof DOMException && error.name === 'AbortError' ? '响应超时' : '无法连接';
    throw new Error(`RelayOps 地址${suffix}：${relayopsOrigin}。请确认服务已启动、地址填写正确；默认本机端口是 18180。`);
  }
  const health = result?.data ?? result;
  if (!response.ok || health?.service !== 'relayops' || health?.status !== 'ok') {
    throw new Error(`${relayopsOrigin} 可以访问，但不是兼容的 RelayOps 服务。请填写 RelayOps 管理服务地址，而不是上游站点或 NewAPI 地址。`);
  }
}

async function pendingPairing(relayopsOrigin, code) {
  let response;
  let result;
  try {
    ({ response, result } = await fetchJSON(`${relayopsOrigin}/api/v1/pair/${encodeURIComponent(code)}/status`));
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw new Error('检查配对码超时，请确认 RelayOps 服务仍在运行');
    throw new Error('检查配对码时与 RelayOps 连接中断，请稍后重试');
  }
  if (response.status === 404) throw new Error('配对码不存在或输入有误，请回到控制台重新复制');
  if (!response.ok) throw new Error(pairingError(result, response.status));
  const state = result?.data ?? result;
  if (state?.status === 'expired') throw new Error('配对码已过期，请回到控制台重新生成；一次性配对码有效期为 5 分钟');
  if (state?.status === 'completed' || state?.status === 'used') throw new Error('配对码已经使用，不能重复配对，请重新生成');
  if (state?.status === 'failed') {
	throw new Error(`配对码的 ${state.attempt_count || 1} 次尝试均未通过，请重新生成：${pairingClassMessage(state.last_error_class)}`);
  }
  if (state?.status !== 'pending') throw new Error(`配对码当前状态为 ${state?.status || '未知'}，只有待配对状态可以继续`);
  if (state?.attempt_consumed === false && state?.retry_after_seconds > 0) {
	throw new Error(`上一次登录态已提交，但上游只读验证暂时失败；本次未扣配对次数，请等待 ${state.retry_after_seconds} 秒后使用同一配对码重试。`);
  }
  if (!state.expected_origin) {
    throw new Error('当前 RelayOps 版本缺少安全站点核验契约，请更新 RelayOps 后再配对；本次没有读取登录态');
  }
  let expectedOrigin;
  try {
    expectedOrigin = httpOrigin(state.expected_origin);
  } catch {
    throw new Error('RelayOps 返回的目标站点无效，请在控制台检查该上游地址');
  }
  return { ...state, expectedOrigin };
}

async function fetchJSON(url, options = {}, timeoutMs = requestTimeoutMs) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetch(url, { ...options, cache: 'no-store', credentials: 'omit', signal: controller.signal });
    const result = await response.json().catch(() => null);
    return { response, result };
  } finally {
    clearTimeout(timeout);
  }
}

async function submitPairing(relayopsOrigin, code, upstream, payload) {
  return fetchJSON(`${relayopsOrigin}/api/v1/pair/${encodeURIComponent(code)}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: `${upstream.hostname} 登录态`, kind: 'browser_pairing', site_url: upstream.origin, payload }),
  }, pairingSubmitTimeoutMs);
}

async function recoverPairingSubmission(relayopsOrigin, code, submitError) {
  let lastState;
  for (const delayMs of [0, 500, 1500, 3000]) {
    if (delayMs > 0) await new Promise((resolve) => setTimeout(resolve, delayMs));
    let statusResult;
    try {
      statusResult = await fetchJSON(`${relayopsOrigin}/api/v1/pair/${encodeURIComponent(code)}/status`);
    } catch {
      continue;
    }
    const { response, result } = statusResult;
    if (!response.ok) continue;
    lastState = result?.data ?? result;
    if (lastState?.status === 'completed' || lastState?.status === 'used') return true;
    if (lastState?.status === 'failed') {
      throw new Error(pairingClassMessage(lastState.last_error_class) || lastState.last_error_message || '配对验证失败');
    }
    if (lastState?.status !== 'pending') break;
  }
  if (lastState?.status === 'pending') {
	if (lastState?.last_error_class) throw new Error(pairingError({ data: lastState }, 502));
    throw new Error('提交连接中断后，RelayOps 显示配对码仍可用且尚未写入。请确认服务网络正常后，再使用同一配对码重试。');
  }
  if (submitError instanceof DOMException && submitError.name === 'AbortError') {
    throw new Error('配对验证超过 45 秒，且状态回查未确认写入。配对码尚未被重复提交，请检查上游响应后重试。');
  }
  throw new Error('提交配对时与 RelayOps 连接中断，状态回查也未能确认结果。请先恢复 RelayOps 连接，再检查同一配对码状态。');
}

function pairingError(result, status) {
  const detail = result?.data ?? result;
  const classified = pairingClassMessage(detail?.error_class, false);
  if (detail?.attempt_consumed === false) {
	const base = classified || detail?.message || '上游暂时未能完成只读验证';
	const retry = detail?.retry_after_seconds > 0 ? `请等待 ${detail.retry_after_seconds} 秒后使用同一配对码重试` : '可以使用同一配对码重试';
	const preserved = detail?.old_credential_preserved ? '旧凭据和旧数据均已保留' : '本次候选登录态尚未保存';
	return `登录态已经提交到 RelayOps，但${base}。${preserved}，本次不扣配对次数；${retry}。`;
  }
  if (detail?.retryable) {
	const base = classified || detail?.message || '本次只读验证未通过';
	return `${base}。配对码仍有效，还可重试 ${detail.attempts_remaining ?? 0} 次。`;
  }
  if (classified) return classified;
  if (detail?.status === 'expired') return '配对码已过期，请重新生成';
  if (detail?.status === 'completed') return '配对码已经使用，请重新生成';
  const message = detail?.message || detail?.error || result?.message || result?.error;
  return typeof message === 'string' && message.trim() ? message : `配对失败（HTTP ${status}）`;
}

function pairingClassMessage(errorClass, fallback = true) {
  const messages = {
    origin_mismatch: '当前站点与配对码绑定的上游不一致，本次登录态未保存，请打开正确站点并重新生成配对码',
	origin_missing: '浏览器没有提供当前站点信息，请重新打开上游页面后重试',
	credential_missing: '当前页面没有找到可复用登录态，请重新登录上游后重试',
	auth_expired: '捕获的登录态已经失效，请刷新或重新登录上游后重试',
	auth_missing: '上游拒绝了当前登录态，请刷新或重新登录上游后重试',
    permission_denied: '当前上游账号无权读取所需数据，请使用具备只读权限的账号',
    contract_changed: '上游接口已经变化，当前连接器无法验证登录态，请先更新连接器',
	rate_limited: '上游当前限流，配对验证未完成，请稍后重试',
	upstream_5xx: '上游服务当前异常，配对验证未完成，请稍后重试',
	connect_timeout: '连接上游超时，配对验证未完成',
	read_timeout: '读取上游响应超时，配对验证未完成',
	timeout: '验证上游登录态超时，请确认上游可访问后重试',
	connection_reset: '上游某个网络地址在 TLS 或读取阶段重置了连接，RelayOps 未能完成只读验证',
	    connection_error: '验证期间无法连接上游，请确认网络和上游状态',
	    tls_error: '上游 TLS 连接失败，请检查其证书或网络环境',
	    dns_error: '无法解析上游域名，请检查网络或站点地址',
	validation_in_progress: 'RelayOps 正在验证刚提交的登录态，请等待当前验证完成',
  };
  return messages[errorClass] || (fallback ? '先前配对没有完成，请重新生成一次性配对码' : '');
}

async function requestOrigins(origins) {
  const patterns = [...new Set(origins.map((origin) => `${origin}/*`))];
  let approved = false;
  try {
    approved = await chrome.permissions.request({ origins: patterns });
  } catch {
    throw new Error('浏览器未能申请本次所需的两个精确站点权限，请关闭扩展弹窗后重试');
  }
  if (!approved) throw new Error('需要授权访问填写的 RelayOps 地址和当前上游站点，才能执行安全检查与本次配对');
}

function setStatus(message, tone = '') {
  statusOutput.className = tone;
  statusOutput.textContent = message;
}
