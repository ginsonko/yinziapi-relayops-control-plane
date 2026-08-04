// Shared, deliberately narrow capture helper used by the popup and the
// background worker. It only reads the already-open, explicitly authorized
// origin; it never fills credentials or interacts with challenge widgets.
(function installRelayOpsCapture(global) {
  function httpOrigin(raw) {
    const parsed = new URL(raw);
    if (!['http:', 'https:'].includes(parsed.protocol)) throw new Error('not an HTTP page');
    return parsed.origin;
  }

  async function readBrowserStorage() {
    const result = { local: {}, session: {}, indexed: {}, userAgent: navigator.userAgent, authSessionID: '' };
    const timeoutValue = {};
    const settleWithin = (promise, milliseconds, fallback, onLateValue) => new Promise((resolve) => {
      let settled = false;
      const timer = setTimeout(() => {
        if (settled) return;
        settled = true;
        resolve(fallback);
      }, milliseconds);
      Promise.resolve(promise).then((value) => {
        if (settled) {
          if (onLateValue) onLateValue(value);
          return;
        }
        settled = true;
        clearTimeout(timer);
        resolve(value);
      }, () => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve(fallback);
      });
    });
    const canonical = {
      authorization: ['authorization'], access_token: ['access_token', 'accessToken'],
      auth_token: ['auth_token', 'authToken'], session_token: ['session_token', 'sessionToken'],
      refresh_token: ['refresh_token', 'refreshToken'], id_token: ['id_token', 'idToken'],
      user_id: ['user_id', 'userId', 'uid'], expires_at: ['expires_at', 'expiresAt', 'expiry', 'expiration'],
      expires_in: ['expires_in', 'expiresIn'],
    };
    const rejectedKey = /password|passwd|secret|captcha|credit|payment/i;
    const normalize = (value) => String(value || '').replace(/[^a-z0-9]/gi, '').toLowerCase();
    const tokenLike = (value) => {
      const text = String(value ?? '').trim();
      return text.length >= 8 && text.length <= 32768 && !/\s/.test(text) && !/^(null|undefined)$/i.test(text);
    };
    const collect = (output, value, path = [], depth = 0) => {
      if (depth > 8 || value == null || Object.keys(output).length >= 64) return;
      if (Array.isArray(value)) {
        value.slice(0, 128).forEach((item) => collect(output, item, path, depth + 1));
        return;
      }
      if (typeof value === 'object') {
        Object.entries(value).slice(0, 128).forEach(([key, child]) => {
          if (!rejectedKey.test(key)) collect(output, child, [...path, key], depth + 1);
        });
        return;
      }
      const key = String(path.at(-1) || '');
      if (!key || rejectedKey.test(key)) return;
      const normalizedKey = normalize(key);
      const text = String(value).trim();
      if (!text || text.length > 32768) return;
      for (const [target, aliases] of Object.entries(canonical)) {
        if (aliases.some((alias) => normalize(alias) === normalizedKey)) {
          if (!target.includes('token') || tokenLike(text)) output[target] ??= text;
          return;
        }
      }
      const parent = path.slice(0, -1).map(normalize);
      if (normalizedKey === 'id' && parent.some((item) => ['user', 'currentuser', 'account', 'profile', 'member', 'self', 'userinfo'].includes(item))) {
        output.user_id ??= text;
      } else if (normalizedKey === 'sid' && parent.some((item) => ['session', 'auth', 'authentication'].includes(item))) {
        output.session_id ??= text;
      } else if (normalizedKey === 'token' && tokenLike(text)) {
        output.token ??= text;
      }
    };
    const keepStorage = (source) => {
      const output = {};
      for (let index = 0; index < Math.min(source.length, 256); index += 1) {
        const key = source.key(index);
        if (!key || rejectedKey.test(key)) continue;
        const raw = source.getItem(key);
        if (!raw || raw.length > 32768) continue;
        let decoded;
        try { decoded = JSON.parse(raw); } catch { decoded = raw; }
        collect(output, decoded, [key]);
      }
      return output;
    };
    result.local = keepStorage(localStorage);
    result.session = keepStorage(sessionStorage);

    if (indexedDB?.databases) {
      const indexedDeadline = Date.now() + 4_000;
      const operationBudget = () => Math.min(750, Math.max(0, indexedDeadline - Date.now()));
      const databases = await settleWithin(indexedDB.databases(), operationBudget(), []);
      const databaseNames = databases.map((item) => item.name).filter(Boolean).slice(0, 12);
      for (const databaseName of databaseNames) {
        if (operationBudget() <= 0) break;
        try {
          const opening = new Promise((resolve, reject) => {
            const open = indexedDB.open(databaseName);
            open.onsuccess = () => resolve(open.result);
            open.onerror = () => reject(open.error);
            open.onupgradeneeded = () => { open.transaction?.abort(); reject(new Error('database upgrade refused')); };
          });
          const database = await settleWithin(opening, operationBudget(), null, (lateDatabase) => lateDatabase?.close?.());
          if (!database) continue;
          try {
            for (const storeName of Array.from(database.objectStoreNames).slice(0, 12)) {
              if (operationBudget() <= 0) break;
              const transaction = database.transaction(storeName, 'readonly');
              const store = transaction.objectStore(storeName);
              const request = store.openCursor();
              const cursorFinished = new Promise((resolve) => {
                let count = 0;
                request.onsuccess = () => {
                  const cursor = request.result;
                  if (!cursor || count >= 128 || Object.keys(result.indexed).length >= 64) return resolve();
                  count += 1;
                  collect(result.indexed, cursor.value, [String(databaseName), String(storeName), String(cursor.key ?? '')]);
                  cursor.continue();
                };
                request.onerror = () => resolve();
              });
              const cursorResult = await settleWithin(cursorFinished, operationBudget(), timeoutValue);
              if (cursorResult === timeoutValue) {
                try { transaction.abort(); } catch { /* transaction already completed */ }
              }
              if (Object.keys(result.indexed).length >= 64) break;
            }
          } finally { database.close(); }
        } catch { /* blocked, opaque, or changing databases are skipped */ }
        if (Object.keys(result.indexed).length >= 64) break;
      }
    }

    result.authSessionID = result.local.session_id || result.session.session_id || result.indexed.session_id || '';
    delete result.local.session_id;
    delete result.session.session_id;
    delete result.indexed.session_id;

    // NewAPI access tokens can expire while the durable HttpOnly browser
    // session is still valid. A structurally complete token in storage is not
    // proof that it is current, so every demand-driven custody capture tries
    // the same-origin session exchange and replaces only the captured token
    // fields when that exchange succeeds.
    const boundedJSON = async (response) => {
      const maximumBytes = 1024 * 1024;
      const declared = Number.parseInt(response.headers?.get?.('content-length') || '0', 10);
      if (Number.isFinite(declared) && declared > maximumBytes) throw new Error('NewAPI authentication response is too large');
      const text = await response.text();
      if (text.length > maximumBytes) throw new Error('NewAPI authentication response is too large');
      return JSON.parse(text);
    };
    const bridgeNewAPISession = async () => {
      if (typeof fetch !== 'function' || typeof location === 'undefined') return;
      try {
        const boundedFetch = async (url, options) => {
          const controller = new AbortController();
          const timer = setTimeout(() => controller.abort(), 4_000);
          try { return await fetch(url, { ...options, signal: controller.signal }); }
          finally { clearTimeout(timer); }
        };
        const requestOptions = {
          credentials: 'include', mode: 'same-origin', redirect: 'error', cache: 'no-store',
          headers: { Accept: 'application/json', 'Cache-Control': 'no-store' },
        };
        const statusResponse = await boundedFetch(new URL('/api/status', location.origin).href, requestOptions);
        if (!statusResponse.ok) return;
        const status = await boundedJSON(statusResponse);
        if (status?.success !== true || typeof status?.data?.version !== 'string' || typeof status?.data?.system_name !== 'string') return;

        const refreshHeaders = { ...requestOptions.headers };
        if (tokenLike(result.authSessionID)) refreshHeaders['X-Auth-Session'] = result.authSessionID;
        const refreshResponse = await boundedFetch(new URL('/api/user/auth/refresh', location.origin).href, {
          ...requestOptions, method: 'POST', headers: refreshHeaders,
        });
        if (!refreshResponse.ok) return;
        const refresh = await boundedJSON(refreshResponse);
        const explicitFailure = refresh?.success === false ||
          (Object.prototype.hasOwnProperty.call(refresh ?? {}, 'code') && ![0, '0'].includes(refresh.code));
        const bundle = refresh?.data && typeof refresh.data === 'object' ? refresh.data : refresh;
        const token = bundle?.access_token;
        const userID = Number(bundle?.user?.id ?? bundle?.user_id ?? result.local.user_id ?? result.session.user_id ?? result.indexed.user_id);
        if (explicitFailure || !tokenLike(token) || !Number.isInteger(userID) || userID <= 0) return;
        result.local.access_token = token;
        result.local.user_id = String(userID);
        if (Number.isFinite(bundle.access_expires_at) && bundle.access_expires_at > 0) {
          result.local.expires_at = String(bundle.access_expires_at);
        }
      } catch {
        // The normal Cookie/storage candidate is still returned and remains
        // subject to RelayOps' server-side validation and atomic replacement.
      }
    };
    await bridgeNewAPISession();
    return result;
  }

  async function captureLoginState(tabId, upstreamOrigin) {
	const tab = await chrome.tabs.get(tabId);
	if (!tab?.url || httpOrigin(tab.url) !== upstreamOrigin) throw new Error('opened page no longer matches the scoped upstream origin');
	const cookies = await chrome.cookies.getAll({ url: tab.url });
    const [{ result: storage }] = await chrome.scripting.executeScript({
      target: { tabId },
      func: readBrowserStorage,
    });
    const payload = {
      captured_origin: upstreamOrigin,
	  cookie: [...cookies].sort((left, right) => {
		const leftKey = `${left.domain || ''}\n${left.path || ''}\n${left.name || ''}`;
		const rightKey = `${right.domain || ''}\n${right.path || ''}\n${right.name || ''}`;
		return leftKey < rightKey ? -1 : leftKey > rightKey ? 1 : 0;
	  }).map((item) => `${item.name}=${item.value}`).join('; '),
      local_storage: storage?.local ?? {},
      session_storage: { ...(storage?.session ?? {}), ...(storage?.indexed ?? {}) },
      headers: storage?.userAgent ? { 'User-Agent': storage.userAgent } : {},
    };
    const captured = payload.cookie || Object.keys(payload.local_storage).length > 0 || Object.keys(payload.session_storage).length > 0;
    if (!captured) throw new Error('当前页面没有可复用的登录态；请完成登录或人机验证并等待页面加载完成');
    return payload;
  }

  function canonicalize(value) {
	if (Array.isArray(value)) return value.map(canonicalize);
	if (value && typeof value === 'object') {
	  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, canonicalize(value[key])]));
	}
	return value;
  }

  async function fingerprintLoginState(payload) {
	if (!global.crypto?.subtle || typeof global.TextEncoder !== 'function') {
	  throw new Error('当前浏览器不支持安全会话指纹，请升级 Chrome 或 Edge');
	}
	const canonical = JSON.stringify(canonicalize({
	  captured_origin: payload?.captured_origin || '',
	  cookie: payload?.cookie || '',
	  local_storage: payload?.local_storage || {},
	  session_storage: payload?.session_storage || {},
	  user_agent: payload?.headers?.['User-Agent'] || '',
	}));
	const digest = await global.crypto.subtle.digest('SHA-256', new global.TextEncoder().encode(canonical));
	return Array.from(new Uint8Array(digest)).map((value) => value.toString(16).padStart(2, '0')).join('');
  }

  global.RelayOpsCapture = { httpOrigin, captureLoginState, fingerprintLoginState, readBrowserStorage };
})(globalThis);
