import assert from 'node:assert/strict';
import test from 'node:test';
import { projectSupplierCredentialBindings } from '../src/pages/credentialProjection.ts';

const supplier = (id: string, credentialId: string | null, name = 'same') => ({ id, credentialId, name });
const credential = (id: string, supplierName = 'same') => ({ id, supplier: supplierName });

test('AIZZZ exact binding 3 -> 9 defeats earlier same-name and same-origin credential 22', () => {
	const earlier = credential('22', 'AIZZZ');
	const exact = credential('9', 'AIZZZ');
	const result = projectSupplierCredentialBindings([supplier('3', '9', 'AIZZZ')], [earlier, exact]);
	assert.equal(result.bindings[0].state, 'bound');
	assert.equal(result.bindings[0].credential, exact);
	assert.deepEqual(result.orphans, [earlier]);
});

test('missing, malformed, dangling and duplicate identities fail closed without name fallback', () => {
	const rows = [credential('9'), credential('9')];
	const result = projectSupplierCredentialBindings([
		supplier('missing', null),
		supplier('malformed', '09'),
		supplier('dangling', '10'),
		supplier('duplicate', '9'),
	], rows);
	assert.deepEqual(result.bindings.map((item) => item.state), ['unconfigured', 'unconfigured', 'unconfigured', 'ambiguous']);
	assert.ok(result.bindings.every((item) => item.credential === undefined));
	assert.deepEqual(result.orphans, []);
});

test('a shared exact credential projects once per supplier and is not also orphaned', () => {
	const shared = credential('9');
	const result = projectSupplierCredentialBindings([supplier('3', '9'), supplier('4', '9')], [shared]);
	assert.deepEqual(result.bindings.map((item) => item.credential), [shared, shared]);
	assert.deepEqual(result.orphans, []);
});

test('a truly unreferenced credential remains one orphan', () => {
	const orphan = credential('22');
	const result = projectSupplierCredentialBindings([supplier('3', null)], [orphan]);
	assert.equal(result.bindings[0].state, 'unconfigured');
	assert.deepEqual(result.orphans, [orphan]);
});
