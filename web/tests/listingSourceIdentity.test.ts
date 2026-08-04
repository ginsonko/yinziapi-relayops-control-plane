import assert from 'node:assert/strict';
import test from 'node:test';
import {
	normalizeResolvedListingSource,
	parseCanonicalGroupToken,
	parseCanonicalSupplierID,
	projectListingPreviewIdentity,
	projectListingSourceForRender,
	resolveCatalogGroupIdentity,
	resolveSupplierIdentity,
	runAfterCanonicalGroupToken,
	runAfterCanonicalSupplierID,
	verifyCatalogSnapshotOwnership,
} from '../src/pages/listingSourceIdentity.ts';

const supplier = (id: unknown, name: string) => ({ id, name });
const group = (external_id: unknown, name: string, model_ids: string[] = []) => ({ external_id, name, model_ids });
const goTrimSpaceCodePoints = [
	...Array.from({ length: 5 }, (_, index) => 0x0009 + index),
	0x0020, 0x0085, 0x00a0, 0x1680,
	...Array.from({ length: 11 }, (_, index) => 0x2000 + index),
	0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
];

const codePointVector = (value: string) => Array.from(value, (part) => part.codePointAt(0));

test('canonical Supplier IDs accept only positive safe integers and canonical decimal strings', () => {
	for (const input of [1, Number.MAX_SAFE_INTEGER, '1', String(Number.MAX_SAFE_INTEGER)]) {
		assert.equal(parseCanonicalSupplierID(input).status, 'resolved', String(input));
	}
	for (const input of [undefined, null, '', 0, -1, 1.5, NaN, Infinity, '0', '01', '+1', '-1', '1.0', '1e3', ' 1', '1 ', 'x', String(Number.MAX_SAFE_INTEGER + 1)]) {
		assert.notEqual(parseCanonicalSupplierID(input).status, 'resolved', String(input));
	}
});

test('exact Supplier ID defeats an earlier same-name row and normalizes number/string DTOs', () => {
	const earlier = supplier(8, 'same');
	const exact = supplier('17', 'same');
	const result = resolveSupplierIdentity([earlier, exact], { supplierId: 17, supplierName: 'same', requireExact: true });
	assert.equal(result.status, 'resolved');
	if (result.status === 'resolved') assert.equal(result.value, exact);
});

test('missing, stale, malformed, and duplicate exact Supplier IDs fail closed without name fallback', () => {
	const rows = [supplier(17, 'same'), supplier('17', 'same')];
	assert.equal(resolveSupplierIdentity(rows, { supplierName: 'same', requireExact: true }).status, 'missing_identifier');
	assert.equal(resolveSupplierIdentity(rows, { supplierId: 18, supplierName: 'same', requireExact: true }).status, 'not_found');
	assert.equal(resolveSupplierIdentity(rows, { supplierId: '017', supplierName: 'same', requireExact: true }).status, 'invalid_identifier');
	const duplicate = resolveSupplierIdentity(rows, { supplierId: 17, supplierName: 'same', requireExact: true });
	assert.equal(duplicate.status, 'ambiguous');
	if (duplicate.status === 'ambiguous') assert.equal(duplicate.matchCount, 2);
});

test('legacy Supplier name compatibility requires exactly one array slot', () => {
	const one = supplier(17, 'legacy');
	assert.equal(resolveSupplierIdentity([one], { supplierName: 'legacy' }).status, 'resolved');
	assert.equal(resolveSupplierIdentity([one, one], { supplierName: 'legacy' }).status, 'ambiguous');
	assert.equal(resolveSupplierIdentity([supplier(17, 'legacy'), supplier(18, 'legacy')], { supplierName: 'legacy' }).status, 'ambiguous');
});

test('exact group ID defeats same-name and external-ID/name cross collisions', () => {
	const sameName = group('other', 'Jimeng');
	const exact = group('Jimeng', 'Current Jimeng', ['video-a']);
	const result = resolveCatalogGroupIdentity([sameName, exact], { groupExternalId: 'Jimeng', groupName: 'Jimeng', requireExact: true });
	assert.equal(result.status, 'resolved');
	if (result.status === 'resolved') assert.equal(result.value, exact);
});

test('exact group IDs reject outer whitespace, preserve case and internal spaces, and never fall back', () => {
	const rows = [group('Jimeng', 'legacy-name'), group('Ji meng', 'internal-space')];
	assert.equal(resolveCatalogGroupIdentity(rows, { groupExternalId: '', groupName: 'legacy-name', requireExact: true }).status, 'missing_identifier');
	assert.equal(resolveCatalogGroupIdentity(rows, { groupExternalId: 'jimeng', groupName: 'legacy-name', requireExact: true }).status, 'not_found');
	assert.equal(resolveCatalogGroupIdentity(rows, { groupExternalId: ' Jimeng', groupName: 'legacy-name', requireExact: true }).status, 'invalid_identifier');
	assert.equal(resolveCatalogGroupIdentity(rows, { groupExternalId: 'Jimeng ', groupName: 'legacy-name', requireExact: true }).status, 'invalid_identifier');
	assert.equal(resolveCatalogGroupIdentity(rows, { groupExternalId: 'Ji meng', requireExact: true }).status, 'resolved');
});

test('canonical group tokens reject the complete Go 1.25.1 TrimSpace boundary corpus', () => {
	const exactRow = group('valid', 'valid');
	for (const codePoint of goTrimSpaceCodePoints) {
		const rune = String.fromCodePoint(codePoint);
		for (const token of [`${rune}valid`, `valid${rune}`]) {
			assert.equal(parseCanonicalGroupToken(token).status, 'invalid_identifier', `U+${codePoint.toString(16)} parser`);
			assert.equal(resolveCatalogGroupIdentity([exactRow], { groupExternalId: token, groupName: 'valid', requireExact: true }).status, 'invalid_identifier', `U+${codePoint.toString(16)} exact`);
			assert.equal(resolveCatalogGroupIdentity([exactRow], { groupName: token }).status, 'invalid_identifier', `U+${codePoint.toString(16)} legacy`);
		}
	}
});

test('U+0085 is rejected at a boundary even though Node trim preserves it', () => {
	const token = `valid${String.fromCodePoint(0x0085)}`;
	assert.equal(token.trim(), token);
	assert.equal(parseCanonicalGroupToken(token).status, 'invalid_identifier');
});

test('non-Go-space U+180E, U+200B, and U+FEFF remain exact at boundaries', () => {
	for (const codePoint of [0x180e, 0x200b, 0xfeff]) {
		const rune = String.fromCodePoint(codePoint);
		for (const token of [`${rune}valid`, `valid${rune}`]) {
			const parsed = parseCanonicalGroupToken(token);
			assert.equal(parsed.status, 'resolved', `U+${codePoint.toString(16)}`);
			if (parsed.status === 'resolved') assert.deepEqual(codePointVector(parsed.value), codePointVector(token));
		}
	}
});

test('Go-space runes remain exact when internal', () => {
	for (const codePoint of goTrimSpaceCodePoints) {
		const token = `left${String.fromCodePoint(codePoint)}right`;
		const parsed = parseCanonicalGroupToken(token);
		assert.equal(parsed.status, 'resolved', `U+${codePoint.toString(16)}`);
		assert.equal(resolveCatalogGroupIdentity([group(token, token)], { groupExternalId: token, requireExact: true }).status, 'resolved');
	}
});

test('NFC and NFD group tokens remain distinct', () => {
	const nfc = '\u00e9';
	const nfd = 'e\u0301';
	assert.notDeepEqual(codePointVector(nfc), codePointVector(nfd));
	const rows = [group(nfc, 'NFC'), group(nfd, 'NFD')];
	const nfcResult = resolveCatalogGroupIdentity(rows, { groupExternalId: nfc, requireExact: true });
	const nfdResult = resolveCatalogGroupIdentity(rows, { groupExternalId: nfd, requireExact: true });
	assert.equal(nfcResult.status, 'resolved');
	assert.equal(nfdResult.status, 'resolved');
	if (nfcResult.status === 'resolved') assert.equal(nfcResult.value.name, 'NFC');
	if (nfdResult.status === 'resolved') assert.equal(nfdResult.value.name, 'NFD');
});

test('an own explicit group ID never falls back when undefined, null, empty, or invalid', () => {
	const rows = [group('legacy', 'legacy')];
	for (const groupExternalId of [undefined, null, '', ' legacy', 'legacy ']) {
		const result = resolveCatalogGroupIdentity(rows, { groupExternalId, groupName: 'legacy' });
		assert.notEqual(result.status, 'resolved', String(groupExternalId));
	}
	assert.equal(resolveCatalogGroupIdentity(rows, { groupName: 'legacy' }).status, 'resolved');
});

test('invalid legacy tokens do not enter external-ID or display-name fallback', () => {
	const rows = [group(' legacy', ' legacy'), group('legacy ', 'legacy ')];
	assert.equal(resolveCatalogGroupIdentity(rows, { groupName: ' legacy' }).status, 'invalid_identifier');
	assert.equal(resolveCatalogGroupIdentity(rows, { groupName: 'legacy ' }).status, 'invalid_identifier');
	assert.equal(resolveCatalogGroupIdentity(rows, { groupName: '' }).status, 'missing_identifier');
});

test('legacy group OR matching counts array positions, not object or field identity', () => {
	const both = group('legacy', 'legacy');
	const unique = resolveCatalogGroupIdentity([both], { groupName: 'legacy' });
	assert.equal(unique.status, 'resolved');
	const repeatedReference = resolveCatalogGroupIdentity([both, both], { groupName: 'legacy' });
	assert.equal(repeatedReference.status, 'ambiguous');
	if (repeatedReference.status === 'ambiguous') assert.equal(repeatedReference.matchCount, 2);
	const crossCollision = resolveCatalogGroupIdentity(
		[group('legacy', 'first'), group('second', 'legacy')],
		{ groupName: 'legacy' },
	);
	assert.equal(crossCollision.status, 'ambiguous');
});

test('duplicate exact group IDs are ambiguous in every order', () => {
	const first = group('same-id', 'first');
	const second = group('same-id', 'second');
	for (const rows of [[first, second], [second, first]]) {
		const result = resolveCatalogGroupIdentity(rows, { groupExternalId: 'same-id', requireExact: true });
		assert.equal(result.status, 'ambiguous');
		if (result.status === 'ambiguous') assert.equal(result.matchCount, 2);
	}
});

test('successful exact identity is unchanged by array reorder', () => {
	const expectedSupplier = supplier(17, 'expected');
	const expectedGroup = group('Jimeng', 'expected', ['video-a']);
	const suppliers = [supplier(8, 'expected'), expectedSupplier, supplier(19, 'other')];
	const groups = [group('other', 'Jimeng'), expectedGroup, group('last', 'last')];
	for (const supplierRows of [suppliers, [...suppliers].reverse()]) {
		const result = resolveSupplierIdentity(supplierRows, { supplierId: '17', requireExact: true });
		assert.equal(result.status, 'resolved');
		if (result.status === 'resolved') assert.equal(result.value, expectedSupplier);
	}
	for (const groupRows of [groups, [...groups].reverse()]) {
		const result = resolveCatalogGroupIdentity(groupRows, { groupExternalId: 'Jimeng', requireExact: true });
		assert.equal(result.status, 'resolved');
		if (result.status === 'resolved') assert.equal(result.value, expectedGroup);
	}
});

test('snapshot ownership uses the same canonical Supplier ID contract', () => {
	const snapshot = { supplier_id: '17', groups: [], models: [] };
	assert.equal(verifyCatalogSnapshotOwnership(snapshot, 17).status, 'resolved');
	assert.equal(verifyCatalogSnapshotOwnership(snapshot, 18).status, 'ownership_mismatch');
	assert.equal(verifyCatalogSnapshotOwnership({ ...snapshot, supplier_id: '017' }, 17).status, 'ownership_mismatch');
	assert.equal(verifyCatalogSnapshotOwnership(snapshot, '017').status, 'invalid_identifier');
});

test('one resolved source drives render models and preview exact identity', () => {
	const resolved = normalizeResolvedListingSource({
		sourceKind: 'catalog_group',
		supplier: supplier('17', 'AIZZZ'),
		group: group('Jimeng', '即梦', ['jimeng-video-1', 'jimeng-video-2']),
		evidence: { sourceSnapshotHash: 'sha256:catalog', sourceEffectiveRatio: '1', sourceObservedAt: '2026-07-27T00:00:00Z' },
	});
	assert.equal(resolved.status, 'resolved');
	if (resolved.status !== 'resolved') return;
	const render = projectListingSourceForRender(resolved.value);
	const preview = projectListingPreviewIdentity(resolved.value);
	assert.deepEqual(render.modelIds, ['jimeng-video-1', 'jimeng-video-2']);
	assert.equal(render.groupExternalId, preview.discovery_external_id);
	assert.equal(render.supplierId, preview.supplier_id);
	assert.equal(render.supplierName, 'AIZZZ');
	assert.equal(render.groupName, '即梦');
	assert.ok(Object.isFrozen(resolved.value));
	assert.ok(Object.isFrozen(resolved.value.modelIds));
	assert.ok(Object.isFrozen(resolved.value.evidence));
});

test('preview identity preserves every accepted group token code point', () => {
	const accepted = [
		'plain-token', 'Mixed CASE !@#', '\u00e9', 'e\u0301',
		`${String.fromCodePoint(0x180e)}prefix`, `suffix${String.fromCodePoint(0x200b)}`,
		`left${String.fromCodePoint(0x0085)}right`, `${String.fromCodePoint(0xfeff)}bom`,
	];
	for (const token of accepted) {
		const resolved = normalizeResolvedListingSource({
			sourceKind: 'group', supplier: supplier(17, 'supplier'), group: group(token, token, ['model']),
		});
		assert.equal(resolved.status, 'resolved', JSON.stringify(token));
		if (resolved.status !== 'resolved') continue;
		const preview = projectListingPreviewIdentity(resolved.value);
		assert.deepEqual(codePointVector(preview.discovery_external_id), codePointVector(token));
	}
});

test('catalog resolved source requires frozen hash and effective ratio evidence', () => {
	const input = {
		sourceKind: 'catalog_group' as const,
		supplier: supplier(17, 'AIZZZ'),
		group: group('Jimeng', '即梦', ['video']),
	};
	assert.equal(normalizeResolvedListingSource({ ...input, evidence: { sourceSnapshotHash: '', sourceEffectiveRatio: '1' } }).status, 'missing_identifier');
	assert.equal(normalizeResolvedListingSource({ ...input, evidence: { sourceSnapshotHash: 'hash', sourceEffectiveRatio: '' } }).status, 'missing_identifier');
});

test('outer-whitespace group identity cannot reach the preview projection used by the trimming backend', () => {
	const selected = resolveCatalogGroupIdentity(
		[group(' x ', 'spaced'), group('x', 'plain')],
		{ groupExternalId: ' x ', requireExact: true },
	);
	assert.equal(selected.status, 'invalid_identifier');
	const normalized = normalizeResolvedListingSource({
		sourceKind: 'catalog_group',
		supplier: supplier(17, 'AIZZZ'),
		group: group(' x ', 'spaced', ['video']),
		evidence: { sourceSnapshotHash: 'hash', sourceEffectiveRatio: '1' },
	});
	assert.equal(normalized.status, 'invalid_identifier');
});

test('parse-before-effect invokes effects only for valid canonical Supplier IDs', async () => {
	let calls = 0;
	const effect = (id: number) => { calls += 1; return `supplier:${id}`; };
	for (const input of [undefined, null, '', 0, -1, 1.5, NaN, Infinity, '01', '+1', '1e3', ' 1', String(Number.MAX_SAFE_INTEGER + 1)]) {
		const before = calls;
		const result = await runAfterCanonicalSupplierID(input, effect);
		assert.notEqual(result.status, 'resolved', String(input));
		assert.equal(calls, before, String(input));
	}
	const result = await runAfterCanonicalSupplierID('17', effect);
	assert.equal(result.status, 'resolved');
	if (result.status === 'resolved') assert.equal(result.value, 'supplier:17');
	assert.equal(calls, 1);
});

test('group parse-before-effect invokes zero invalid effects and one valid effect', async () => {
	let calls = 0;
	const effect = (token: string) => { calls += 1; return codePointVector(token); };
	const invalid = [undefined, null, '', 17, ...goTrimSpaceCodePoints.flatMap((codePoint) => {
		const rune = String.fromCodePoint(codePoint);
		return [`${rune}valid`, `valid${rune}`];
	})];
	for (const token of invalid) {
		const before = calls;
		const result = await runAfterCanonicalGroupToken(token, effect);
		assert.notEqual(result.status, 'resolved', String(token));
		assert.equal(calls, before, String(token));
	}
	const valid = `left${String.fromCodePoint(0x0085)}right`;
	const result = await runAfterCanonicalGroupToken(valid, effect);
	assert.equal(result.status, 'resolved');
	if (result.status === 'resolved') assert.deepEqual(result.value, codePointVector(valid));
	assert.equal(calls, 1);
});
