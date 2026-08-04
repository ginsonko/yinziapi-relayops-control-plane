import assert from 'node:assert/strict';
import test from 'node:test';
import { normalizeUpstreamKeyManagementPath, upstreamKeyManagementLinks } from '../src/pages/upstreamKeyManagement.ts';

test('normalizes safe Unicode and space paths into backend-compatible canonical encoding', () => {
	assert.equal(normalizeUpstreamKeyManagementPath('/控制台/密钥'), '/%E6%8E%A7%E5%88%B6%E5%8F%B0/%E5%AF%86%E9%92%A5');
	assert.equal(normalizeUpstreamKeyManagementPath('/%E6%8E%A7%E5%88%B6%E5%8F%B0/%E5%AF%86%E9%92%A5'), '/%E6%8E%A7%E5%88%B6%E5%8F%B0/%E5%AF%86%E9%92%A5');
	assert.equal(normalizeUpstreamKeyManagementPath('/account keys'), '/account%20keys');
	assert.equal(normalizeUpstreamKeyManagementPath('/account%20keys'), '/account%20keys');
	assert.equal(normalizeUpstreamKeyManagementPath('/'), '/');
	assert.equal(normalizeUpstreamKeyManagementPath(`/${'界'.repeat(171)}`), null);
});

test('uses a backend-normalized Unicode path as the exact configured destination', () => {
	const result = upstreamKeyManagementLinks({
		base_url: 'https://modified.example/pricing',
		adapter: 'json',
		metadata: { api_key_management_path: '/%E6%8E%A7%E5%88%B6%E5%8F%B0/%E5%AF%86%E9%92%A5' },
	});
	assert.deepEqual(result?.options, [{
		url: 'https://modified.example/%E6%8E%A7%E5%88%B6%E5%8F%B0/%E5%AF%86%E9%92%A5',
		label: '打开已配置的 Key 管理页',
		exact: true,
	}]);
});

test('uses one explicitly configured same-origin path', () => {
	const result = upstreamKeyManagementLinks({
		base_url: 'https://modified.example/pricing',
		adapter: 'json',
		metadata: { api_key_management_path: '/account/keys' },
	});
	assert.deepEqual(result, {
		options: [{ url: 'https://modified.example/account/keys', label: '打开已配置的 Key 管理页', exact: true }],
		homeUrl: 'https://modified.example/',
		configured: true,
	});
});

test('offers both first-party NewAPI frontend routes without claiming detection', () => {
	const result = upstreamKeyManagementLinks({ base_url: 'https://newapi.example', adapter: 'newapi' });
	assert.deepEqual(result?.options.map(({ url, exact }) => ({ url, exact })), [
		{ url: 'https://newapi.example/keys', exact: false },
		{ url: 'https://newapi.example/console/token', exact: false },
	]);
	assert.equal(result?.configured, false);
});

for (const adapter of ['sub2api', 'sub2api_upstream', 'sub2api_admin']) {
	test(`offers the Sub2 key page for ${adapter}`, () => {
		const result = upstreamKeyManagementLinks({ base_url: 'https://sub2.example/dashboard', adapter });
		assert.deepEqual(result?.options.map(({ url, exact }) => ({ url, exact })), [
			{ url: 'https://sub2.example/keys', exact: false },
		]);
	});
}

test('normalizes harmless adapter whitespace before selecting a native candidate', () => {
	const result = upstreamKeyManagementLinks({ base_url: 'https://sub2.example', adapter: ' SUB2API_UPSTREAM ' });
	assert.equal(result?.options[0]?.url, 'https://sub2.example/keys');
});

test('falls back to the same-origin homepage for unknown adapters', () => {
	const result = upstreamKeyManagementLinks({ base_url: 'https://custom.example/pricing', adapter: 'json' });
	assert.deepEqual(result, { options: [], homeUrl: 'https://custom.example/', configured: false });
});

for (const path of [
	'//evil.example/keys', '/keys?token=secret', '/keys#secret', '/%2e%2e/admin', '/safe/.%2E/admin',
	'/%2F%2Fevil.example/keys', '/safe%2Fkeys', '/safe%252Fkeys', '/safe%5Ckeys', '/safe%3Ftoken', '/safe%23fragment',
	'/safe%0Akeys', '/keys//', '/%ZZ', `/${'a'.repeat(513)}`,
]) {
	test(`rejects unsafe configured path ${path} without leaving the supplier origin`, () => {
		const result = upstreamKeyManagementLinks({
			base_url: 'https://safe.example/pricing',
			adapter: 'newapi',
			metadata: { api_key_management_path: path },
		});
		assert.deepEqual(result, { options: [], homeUrl: 'https://safe.example/', configured: false });
	});
}

test('rejects non-http and credential-bearing supplier URLs', () => {
	assert.equal(upstreamKeyManagementLinks({ base_url: 'file:///tmp/site', adapter: 'newapi' }), null);
	assert.equal(upstreamKeyManagementLinks({ base_url: 'https://user:pass@example.com', adapter: 'newapi' }), null);
});
