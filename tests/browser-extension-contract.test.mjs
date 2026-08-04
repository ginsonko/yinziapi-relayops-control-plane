import assert from 'node:assert/strict';
import { webcrypto } from 'node:crypto';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const root = new URL('../browser-extension/', import.meta.url);

function load(name, additions = {}) {
  const context = vm.createContext({ URL, AbortController, setTimeout, clearTimeout, ...additions });
  vm.runInContext(fs.readFileSync(new URL(name, root), 'utf8'), context, { filename: name });
  return context;
}

test('custody client uses fixed routes with one API prefix', async () => {
  let request;
  const context = load('custody-client.js', {
    chrome: { runtime: { getManifest: () => ({ version: '1.3.8' }) } },
    fetch: async (url, options) => {
      request = { url, options };
      return { ok: true, status: 200, json: async () => ({ success: true }) };
    },
  });
  const client = context.RelayOpsCustody;
  assert.equal(client.routeURL('http://127.0.0.1:18202/path', 'status'), 'http://127.0.0.1:18202/api/v1/browser-custody/status');
  assert.throws(() => client.routeURL('http://127.0.0.1:18202', '../admin'), /unsupported custody route/);
  assert.throws(() => client.routeURL('file:///tmp/relayops', 'status'), /HTTP origin/);
	assert.throws(() => client.routeURL('http://relayops.example:18202', 'status'), /必须使用 HTTPS/);
	assert.throws(() => client.routeURL('http://10.0.0.8:18180', 'status'));
	assert.throws(() => client.routeURL('http://100.63.255.255:18180', 'status'));
	assert.throws(() => client.routeURL('http://100.128.0.1:18180', 'status'));
	assert.equal(client.routeURL('http://100.64.0.1:18180/path', 'status'), 'http://100.64.0.1:18180/api/v1/browser-custody/status');
	assert.equal(client.routeURL('http://100.127.255.254:18180', 'status'), 'http://100.127.255.254:18180/api/v1/browser-custody/status');
	assert.equal(client.routeURL('https://relayops.example/path', 'status'), 'https://relayops.example/api/v1/browser-custody/status');
  await client.request('http://127.0.0.1:18202', 'capture', 'scoped-token', { method: 'POST' });
  assert.equal(request.url, 'http://127.0.0.1:18202/api/v1/browser-custody/capture');
  assert.equal(request.options.headers['X-RelayOps-Custody'], 'scoped-token');
  assert.equal(request.options.headers['X-RelayOps-Helper-Version'], '1.3.8');
  assert.equal(request.url.match(/\/api\/v1/g).length, 1);
});

test('packaged helper contract is version-aligned and loads the shared client', () => {
	const manifest = JSON.parse(fs.readFileSync(new URL('manifest.json', root), 'utf8'));
	const popup = fs.readFileSync(new URL('popup.html', root), 'utf8');
	const popupScript = fs.readFileSync(new URL('popup.js', root), 'utf8');
	const background = fs.readFileSync(new URL('background.js', root), 'utf8');
	assert.equal(manifest.version, '1.3.8');
	assert.match(popup, /capture\.js[\s\S]*custody-client\.js[\s\S]*popup\.js/);
	assert.match(popupScript, /RelayOpsCustody\.relayOpsOrigin\(raw\)/);
	assert.match(background, /importScripts\('capture\.js', 'custody-client\.js'\)/);
});

test('storage capture flattens credential scalars and excludes secret/profile fields', async () => {
  const storage = (values) => ({
    get length() { return Object.keys(values).length; },
    key: (index) => Object.keys(values)[index] ?? null,
    getItem: (key) => values[key] ?? null,
  });
	const context = load('capture.js', {
	crypto: webcrypto,
	TextEncoder,
    navigator: { userAgent: 'RelayOps-Test' },
    localStorage: storage({
      currentUser: JSON.stringify({ profile: { id: 42, nickname: 'private-name' }, accessToken: 'valid-access-token', password: 'must-not-leak' }),
      theme: 'dark',
    }),
    sessionStorage: storage({ auth: JSON.stringify({ refreshToken: 'valid-refresh-token', credit: 999 }) }),
    indexedDB: { databases: async () => [] },
  });
  const result = await context.RelayOpsCapture.readBrowserStorage();
  assert.equal(result.local.access_token, 'valid-access-token');
  assert.equal(result.local.user_id, '42');
  assert.equal(result.session.refresh_token, 'valid-refresh-token');
  const serialized = JSON.stringify(result);
  assert.doesNotMatch(serialized, /must-not-leak|private-name|999|dark/);
	const first = await context.RelayOpsCapture.fingerprintLoginState({ captured_origin: 'https://upstream.example', local_storage: { b: '2', a: '1' } });
	const reordered = await context.RelayOpsCapture.fingerprintLoginState({ local_storage: { a: '1', b: '2' }, captured_origin: 'https://upstream.example' });
	const changed = await context.RelayOpsCapture.fingerprintLoginState({ captured_origin: 'https://upstream.example', local_storage: { a: '1', b: '3' } });
	assert.equal(first, reordered);
	assert.notEqual(first, changed);
});

test('storage capture skips an IndexedDB open that never completes', async () => {
  const values = { auth: JSON.stringify({ user: { id: 52 }, accessToken: 'retained-access-token' }) };
  const storage = {
    get length() { return Object.keys(values).length; },
    key: (index) => Object.keys(values)[index] ?? null,
    getItem: (key) => values[key] ?? null,
  };
  const context = load('capture.js', {
    crypto: webcrypto,
    TextEncoder,
    navigator: { userAgent: 'RelayOps-Test' },
    localStorage: storage,
    sessionStorage: { length: 0, key: () => null, getItem: () => null },
    indexedDB: { databases: async () => [{ name: 'stuck' }], open: () => ({}) },
    setTimeout: (fn) => { queueMicrotask(fn); return 1; },
    clearTimeout: () => {},
  });
  const result = await context.RelayOpsCapture.readBrowserStorage();
  assert.equal(result.local.access_token, 'retained-access-token');
  assert.equal(result.local.user_id, '52');
});

test('a stalled NewAPI session bridge returns the captured candidate', async () => {
  const values = { auth: JSON.stringify({ user: { id: 52 }, accessToken: 'retained-access-token' }) };
  const storage = {
    get length() { return Object.keys(values).length; },
    key: (index) => Object.keys(values)[index] ?? null,
    getItem: (key) => values[key] ?? null,
  };
  const context = load('capture.js', {
    crypto: webcrypto,
    TextEncoder,
    location: { origin: 'https://apiv3.52codeflow.top' },
    navigator: { userAgent: 'RelayOps-Test' },
    localStorage: storage,
    sessionStorage: { length: 0, key: () => null, getItem: () => null },
    indexedDB: { databases: async () => [] },
    fetch: async (_url, options) => new Promise((_resolve, reject) => {
      options.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    }),
    setTimeout: (fn) => { queueMicrotask(fn); return 1; },
    clearTimeout: () => {},
  });
  const result = await context.RelayOpsCapture.readBrowserStorage();
  assert.equal(result.local.access_token, 'retained-access-token');
  assert.equal(result.local.user_id, '52');
});

test('NewAPI stale stored access token is replaced through verified same-origin auth refresh', async () => {
  const values = { currentUser: JSON.stringify({ id: 52, accessToken: 'stale-access-token' }) };
  const storage = {
    get length() { return Object.keys(values).length; },
    key: (index) => Object.keys(values)[index] ?? null,
    getItem: (key) => values[key] ?? null,
  };
  const requests = [];
  const response = (body) => ({
    ok: true,
    headers: { get: () => null },
    text: async () => JSON.stringify(body),
  });
  const context = load('capture.js', {
    crypto: webcrypto,
    TextEncoder,
    location: { origin: 'https://newapi.example' },
    navigator: { userAgent: 'RelayOps-Test' },
    localStorage: storage,
    sessionStorage: storage,
    indexedDB: { databases: async () => [] },
    fetch: async (url, options) => {
      requests.push({ url, options });
      if (url.endsWith('/api/status')) {
        return response({ success: true, data: { version: 'v1.0.0', system_name: 'NewAPI' } });
      }
      return response({ success: true, data: {
        access_token: 'memory-only-access-token', access_expires_at: 2000000000, user: { id: 52, username: 'not-retained' },
      } });
    },
  });
  const result = await context.RelayOpsCapture.readBrowserStorage();
  assert.equal(result.local.access_token, 'memory-only-access-token');
  assert.equal(result.local.user_id, '52');
  assert.equal(result.local.expires_at, '2000000000');
  assert.doesNotMatch(JSON.stringify(result), /not-retained/);
  assert.equal(requests.length, 2);
  assert.equal(requests[0].url, 'https://newapi.example/api/status');
  assert.equal(requests[1].url, 'https://newapi.example/api/user/auth/refresh');
  assert.equal(requests[1].options.method, 'POST');
  assert.equal(requests[1].options.credentials, 'include');
  assert.equal(requests[1].options.mode, 'same-origin');
  assert.equal(requests[1].options.redirect, 'error');
});

test('current 52CodeFlow raw rotation bundle reuses captured identity and sends the scoped session id', async () => {
  const values = { auth: JSON.stringify({ user: { id: 52 }, accessToken: 'stale-access-token', session: { sid: 'scoped-session-id' } }) };
  const storage = {
    get length() { return Object.keys(values).length; },
    key: (index) => Object.keys(values)[index] ?? null,
    getItem: (key) => values[key] ?? null,
  };
  const requests = [];
  const response = (body) => ({
    ok: true,
    headers: { get: () => null },
    text: async () => JSON.stringify(body),
  });
  const context = load('capture.js', {
    crypto: webcrypto,
    TextEncoder,
    location: { origin: 'https://apiv3.52codeflow.top' },
    navigator: { userAgent: 'RelayOps-Test' },
    localStorage: storage,
    sessionStorage: { length: 0, key: () => null, getItem: () => null },
    indexedDB: { databases: async () => [] },
    fetch: async (url, options) => {
      requests.push({ url, options });
      if (url.endsWith('/api/status')) {
        return response({ success: true, data: { version: 'v1.0.0', system_name: '52codeflow' } });
      }
      return response({
        access_token: 'rotated-access-token', token_type: 'Bearer', access_expires_at: 2000000000,
        session: { sid: 'scoped-session-id', expires_at: 2000003600 },
      });
    },
  });
  const result = await context.RelayOpsCapture.readBrowserStorage();
  assert.equal(result.local.access_token, 'rotated-access-token');
  assert.equal(result.local.user_id, '52');
  assert.equal(result.local.expires_at, '2000000000');
  assert.equal(requests[1].options.headers['X-Auth-Session'], 'scoped-session-id');
  assert.equal('session_id' in result.local, false);
  assert.equal('session_id' in result.session, false);
  assert.equal('session_id' in result.indexed, false);
});

test('failed NewAPI session bridge preserves the captured candidate for server validation', async () => {
  const values = { currentUser: JSON.stringify({ id: 52, accessToken: 'captured-access-token' }) };
  const storage = {
    get length() { return Object.keys(values).length; },
    key: (index) => Object.keys(values)[index] ?? null,
    getItem: (key) => values[key] ?? null,
  };
  const context = load('capture.js', {
    crypto: webcrypto,
    TextEncoder,
    location: { origin: 'https://newapi.example' },
    navigator: { userAgent: 'RelayOps-Test' },
    localStorage: storage,
    sessionStorage: { length: 0, key: () => null, getItem: () => null },
    indexedDB: { databases: async () => [] },
    fetch: async () => ({ ok: false, headers: { get: () => null }, text: async () => '' }),
  });
  const result = await context.RelayOpsCapture.readBrowserStorage();
  assert.equal(result.local.access_token, 'captured-access-token');
  assert.equal(result.local.user_id, '52');
});

test('explicitly rejected NewAPI rotation response cannot replace the captured candidate', async () => {
  const values = { currentUser: JSON.stringify({ id: 52, accessToken: 'captured-access-token' }) };
  const storage = {
    get length() { return Object.keys(values).length; },
    key: (index) => Object.keys(values)[index] ?? null,
    getItem: (key) => values[key] ?? null,
  };
  const response = (body) => ({ ok: true, headers: { get: () => null }, text: async () => JSON.stringify(body) });
  const context = load('capture.js', {
    crypto: webcrypto,
    TextEncoder,
    location: { origin: 'https://newapi.example' },
    navigator: { userAgent: 'RelayOps-Test' },
    localStorage: storage,
    sessionStorage: { length: 0, key: () => null, getItem: () => null },
    indexedDB: { databases: async () => [] },
    fetch: async (url) => url.endsWith('/api/status')
      ? response({ success: true, data: { version: 'v1.0.0', system_name: 'NewAPI' } })
      : response({ code: 401, message: 'expired', access_token: 'must-not-replace', user_id: 52 }),
  });
  const result = await context.RelayOpsCapture.readBrowserStorage();
  assert.equal(result.local.access_token, 'captured-access-token');
  assert.equal(result.local.user_id, '52');
});

test('capture uses the current same-origin page URL for path-scoped cookies', async () => {
  let cookieQuery;
  const context = load('capture.js', {
    chrome: {
      tabs: { get: async () => ({ url: 'https://upstream.example/dashboard/overview' }) },
      cookies: { getAll: async (query) => { cookieQuery = query; return [{ name: 'session', value: 'ok', path: '/' }, { name: 'alpha', value: '1', path: '/' }]; } },
      scripting: { executeScript: async () => [{ result: { local: { access_token: 'valid-token' }, session: {}, indexed: {}, userAgent: 'UA' } }] },
    },
  });
  const payload = await context.RelayOpsCapture.captureLoginState(7, 'https://upstream.example');
  assert.equal(cookieQuery.url, 'https://upstream.example/dashboard/overview');
	assert.equal(payload.cookie, 'alpha=1; session=ok');
  await assert.rejects(() => context.RelayOpsCapture.captureLoginState(7, 'https://wrong.example'), /no longer matches/);
});

test('background reads the page only after the server explicitly requests capture', async () => {
  const listeners = {};
	const pendingKey = 'pending:https://upstream.example';
	const state = { browser_custody_bindings: { [pendingKey]: { token: 'scoped', relayops_origin: 'http://127.0.0.1:18202', expected_origin: 'https://upstream.example' } } };
  let needsCapture = false;
	let statusUnauthorized = false;
	let captureMode = 'reject';
	let candidateCookie = 'session=old';
  let captureReads = 0;
  let capturePosts = 0;
  const context = vm.createContext({
    URL, setTimeout, clearTimeout,
    importScripts: () => {},
    RelayOpsCapture: {
      httpOrigin: (raw) => new URL(raw).origin,
	  captureLoginState: async () => { captureReads += 1; return { captured_origin: 'https://upstream.example', cookie: candidateCookie }; },
	  fingerprintLoginState: async (payload) => payload.cookie,
    },
    RelayOpsCustody: {
      request: async (_origin, route) => {
		if (route === 'status') return statusUnauthorized
		  ? { response: { ok: false, status: 401 }, result: { message: 'revoked' } }
		  : { response: { ok: true, status: 200 }, result: { supplier_id: 7, needs_capture: needsCapture, helper_state: 'online' } };
        capturePosts += 1;
		return captureMode === 'reject'
		  ? { response: { ok: false, status: 422 }, result: { message: 'page session is still expired', old_credential_preserved: true } }
		  : { response: { ok: true, status: 200 }, result: { captured: true } };
      },
    },
    chrome: {
      runtime: { onInstalled: { addListener: (fn) => { listeners.installed = fn; } }, onStartup: { addListener: (fn) => { listeners.startup = fn; } }, onMessage: { addListener: (fn) => { listeners.message = fn; } } },
      alarms: { create: async () => {}, onAlarm: { addListener: (fn) => { listeners.alarm = fn; } } },
      storage: { local: { get: async () => state, set: async (value) => Object.assign(state, value) } },
      tabs: {
        query: async () => [{ id: 9, url: 'https://upstream.example/dashboard', lastAccessed: 1 }],
        get: async () => ({ id: 9, url: 'https://upstream.example/dashboard' }),
        onUpdated: { addListener: (fn) => { listeners.updated = fn; } },
        onActivated: { addListener: (fn) => { listeners.activated = fn; } },
      },
    },
  });
  vm.runInContext(fs.readFileSync(new URL('background.js', root), 'utf8'), context, { filename: 'background.js' });
	await context.pollCustodyBindings(true);
  assert.equal(captureReads, 0);
  assert.equal(capturePosts, 0);
	assert.equal(state.browser_custody_bindings[pendingKey], undefined);
	assert.equal(state.browser_custody_bindings['7'].expected_origin, 'https://upstream.example');
	needsCapture = true;
	await context.pollCustodyBindings(true);
  assert.equal(captureReads, 1);
  assert.equal(capturePosts, 1);
	assert.ok(state.browser_custody_bindings['7']);
	await context.pollCustodyBindings(true);
	assert.equal(captureReads, 2);
	assert.equal(capturePosts, 1);
	candidateCookie = 'session=new';
	captureMode = 'success';
	await context.pollCustodyBindings(true);
	assert.equal(captureReads, 3);
	assert.equal(capturePosts, 2);
	assert.equal(state.browser_custody_bindings['7'].last_rejected_capture_fingerprint, '');
	statusUnauthorized = true;
	await context.pollCustodyBindings(true);
	assert.equal(state.browser_custody_bindings['7'], undefined);
  assert.equal(typeof listeners.updated, 'function');
  assert.equal(typeof listeners.alarm, 'function');
});

test('background opens one owned upstream tab and renews the same binding twice', async () => {
  const listeners = {};
  const state = { browser_custody_bindings: { '7': { token: 'scoped', relayops_origin: 'http://127.0.0.1:18202', expected_origin: 'https://upstream.example' } } };
  const tabs = new Map();
  let needsCapture = false;
  let created = 0;
  let removed = 0;
  let posts = 0;
  const context = vm.createContext({
    URL, setTimeout, clearTimeout,
    importScripts: () => {},
    RelayOpsCapture: {
      httpOrigin: (raw) => new URL(raw).origin,
      captureLoginState: async (tabID) => ({ captured_origin: 'https://upstream.example', cookie: `session-${tabID}` }),
      fingerprintLoginState: async (payload) => payload.cookie,
    },
    RelayOpsCustody: {
      request: async (_origin, route) => {
        if (route === 'status') return { response: { ok: true, status: 200 }, result: { supplier_id: 7, needs_capture: needsCapture, helper_state: 'online' } };
        posts += 1;
        needsCapture = false;
        return { response: { ok: true, status: 200 }, result: { captured: true } };
      },
    },
    chrome: {
      runtime: { onInstalled: { addListener: (fn) => { listeners.installed = fn; } }, onStartup: { addListener: (fn) => { listeners.startup = fn; } }, onMessage: { addListener: (fn) => { listeners.message = fn; } } },
      alarms: { create: async () => {}, onAlarm: { addListener: (fn) => { listeners.alarm = fn; } } },
      storage: { local: { get: async () => state, set: async (value) => Object.assign(state, value) } },
      tabs: {
        query: async () => [],
        create: async ({ url }) => { const id = ++created; const tab = { id, url: `${url}dashboard`, status: 'complete', lastAccessed: id }; tabs.set(id, tab); return tab; },
        get: async (id) => { const tab = tabs.get(id); if (!tab) throw new Error('tab closed'); return tab; },
        remove: async (id) => { tabs.delete(id); removed += 1; },
        onUpdated: { addListener: (fn) => { listeners.updated = fn; } },
        onActivated: { addListener: (fn) => { listeners.activated = fn; } },
      },
    },
  });
  vm.runInContext(fs.readFileSync(new URL('background.js', root), 'utf8'), context, { filename: 'background.js' });
  await context.pollCustodyBindings(true);
  assert.equal(posts, 0);
  needsCapture = true;
  await context.pollCustodyBindings(true);
  assert.equal(posts, 1);
  assert.equal(created, 1);
  assert.equal(removed, 1);
  needsCapture = true;
  await context.pollCustodyBindings(true);
  assert.equal(posts, 2);
  assert.equal(created, 2);
  assert.equal(removed, 2);
});

test('background reloads one same-origin page before reusing a rejected session fingerprint', async () => {
	let fresh = false;
	let reads = 0;
	let reloads = 0;
	const context = vm.createContext({
		URL, setTimeout, clearTimeout,
		importScripts: () => {},
		RelayOpsCapture: {
			httpOrigin: (raw) => new URL(raw).origin,
			captureLoginState: async () => { reads += 1; return { captured_origin: 'https://upstream.example', cookie: fresh ? 'session=fresh' : 'session=stale' }; },
			fingerprintLoginState: async (payload) => payload.cookie,
		},
		RelayOpsCustody: { request: async () => ({ response: { ok: true, status: 200 }, result: {} }) },
		chrome: {
			runtime: { onInstalled: { addListener: () => {} }, onStartup: { addListener: () => {} }, onMessage: { addListener: () => {} } },
			alarms: { create: async () => {}, onAlarm: { addListener: () => {} } },
			storage: { local: { get: async () => ({}), set: async () => {} } },
			tabs: {
				get: async () => ({ id: 9, url: 'https://upstream.example/dashboard', status: 'complete' }),
				reload: async () => { reloads += 1; fresh = true; },
				onUpdated: { addListener: () => {} }, onActivated: { addListener: () => {} },
			},
		},
	});
	vm.runInContext(fs.readFileSync(new URL('background.js', root), 'utf8'), context, { filename: 'background.js' });
	const result = await context.captureReadyCandidate({
		expected_origin: 'https://upstream.example',
		last_rejected_capture_at: new Date().toISOString(),
		last_rejected_capture_fingerprint: 'session=stale',
	}, [{ id: 9, url: 'https://upstream.example/dashboard', status: 'complete' }]);
	assert.equal(result.fingerprint, 'session=fresh');
	assert.equal(reads, 2);
	assert.equal(reloads, 1);
});

test('background falls back to another same-origin tab after the first read fails', async () => {
  const listeners = {};
  const state = { browser_custody_bindings: { '7': { token: 'scoped', relayops_origin: 'http://127.0.0.1:18202', expected_origin: 'https://upstream.example' } } };
  let captureReads = 0;
  let capturePosts = 0;
  const context = vm.createContext({
    URL, setTimeout, clearTimeout,
    importScripts: () => {},
    RelayOpsCapture: {
      httpOrigin: (raw) => new URL(raw).origin,
      captureLoginState: async (tabID) => { captureReads += 1; if (tabID === 1) throw new Error('discarded tab'); return { captured_origin: 'https://upstream.example', cookie: 'fresh' }; },
      fingerprintLoginState: async () => 'fresh',
    },
    RelayOpsCustody: {
      request: async (_origin, route) => route === 'status'
        ? { response: { ok: true, status: 200 }, result: { supplier_id: 7, needs_capture: true, helper_state: 'online' } }
        : (++capturePosts, { response: { ok: true, status: 200 }, result: { captured: true } }),
    },
    chrome: {
      runtime: { onInstalled: { addListener: (fn) => { listeners.installed = fn; } }, onStartup: { addListener: (fn) => { listeners.startup = fn; } }, onMessage: { addListener: (fn) => { listeners.message = fn; } } },
      alarms: { create: async () => {}, onAlarm: { addListener: (fn) => { listeners.alarm = fn; } } },
      storage: { local: { get: async () => state, set: async (value) => Object.assign(state, value) } },
      tabs: {
        query: async () => [{ id: 1, url: 'https://upstream.example/old', lastAccessed: 2 }, { id: 2, url: 'https://upstream.example/new', lastAccessed: 1 }],
        get: async (id) => ({ id, url: `https://upstream.example/${id}`, status: 'complete' }),
        remove: async () => {},
        onUpdated: { addListener: (fn) => { listeners.updated = fn; } },
        onActivated: { addListener: (fn) => { listeners.activated = fn; } },
      },
    },
  });
  vm.runInContext(fs.readFileSync(new URL('background.js', root), 'utf8'), context, { filename: 'background.js' });
  await context.pollCustodyBindings(true);
  assert.equal(captureReads, 2);
  assert.equal(capturePosts, 1);
});

test('background capture timeout releases the next custody poll', async () => {
  const listeners = {};
  const state = { browser_custody_bindings: { '7': { token: 'scoped', relayops_origin: 'http://127.0.0.1:18202', expected_origin: 'https://upstream.example' } } };
  let shouldHang = true;
  let statusReads = 0;
  let capturePosts = 0;
  const context = vm.createContext({
    URL,
    setTimeout: (fn, milliseconds, ...args) => setTimeout(fn, Math.min(milliseconds, 10), ...args),
    clearTimeout,
    importScripts: () => {},
    RelayOpsCapture: {
      httpOrigin: (raw) => new URL(raw).origin,
      captureLoginState: async () => shouldHang ? new Promise(() => {}) : { captured_origin: 'https://upstream.example', cookie: 'fresh' },
      fingerprintLoginState: async () => 'fresh',
    },
    RelayOpsCustody: {
      request: async (_origin, route) => {
        if (route === 'status') {
          statusReads += 1;
          return { response: { ok: true, status: 200 }, result: { supplier_id: 7, needs_capture: true, helper_state: 'online' } };
        }
        capturePosts += 1;
        return { response: { ok: true, status: 200 }, result: { captured: true } };
      },
    },
    chrome: {
      runtime: { onInstalled: { addListener: (fn) => { listeners.installed = fn; } }, onStartup: { addListener: (fn) => { listeners.startup = fn; } }, onMessage: { addListener: (fn) => { listeners.message = fn; } } },
      alarms: { create: async () => {}, onAlarm: { addListener: (fn) => { listeners.alarm = fn; } } },
      storage: { local: { get: async () => state, set: async (value) => Object.assign(state, value) } },
      tabs: {
        query: async () => [{ id: 9, url: 'https://upstream.example/dashboard', lastAccessed: 1 }],
        get: async () => ({ id: 9, url: 'https://upstream.example/dashboard', status: 'complete' }),
        onUpdated: { addListener: (fn) => { listeners.updated = fn; } },
        onActivated: { addListener: (fn) => { listeners.activated = fn; } },
      },
    },
  });
  vm.runInContext(fs.readFileSync(new URL('background.js', root), 'utf8'), context, { filename: 'background.js' });
  await context.pollCustodyBindings(true);
  assert.equal(statusReads, 1);
  assert.equal(capturePosts, 0);
  assert.match(state.browser_custody_bindings['7'].last_error, /超时/);
  shouldHang = false;
  await context.pollCustodyBindings(true);
  assert.equal(statusReads, 2);
  assert.equal(capturePosts, 1);
});
