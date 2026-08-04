import assert from 'node:assert/strict';
import test from 'node:test';
import { catalogFreshnessWindowMillis, isCatalogEvidenceFresh } from '../src/pages/catalogListing.ts';

const now = Date.parse('2026-07-26T12:00:00.000Z');

function snapshotAt(observedAt?: string) {
	return {
		supplier_id: 1,
		groups: [],
		models: [],
		evidence: observedAt ? { observed_at: observedAt, response_hash: 'catalog-hash' } : undefined,
	};
}

test('catalog evidence stays fresh through the exact fixed deadline', () => {
	assert.equal(isCatalogEvidenceFresh(snapshotAt(new Date(now - catalogFreshnessWindowMillis).toISOString()), now), true);
	assert.equal(isCatalogEvidenceFresh(snapshotAt(new Date(now - catalogFreshnessWindowMillis - 1).toISOString()), now), false);
});

test('catalog evidence rejects missing, invalid, and excessively future timestamps', () => {
	assert.equal(isCatalogEvidenceFresh(snapshotAt(), now), false);
	assert.equal(isCatalogEvidenceFresh(snapshotAt('not-a-time'), now), false);
	assert.equal(isCatalogEvidenceFresh(snapshotAt(new Date(now + 5 * 60_000 + 1).toISOString()), now), false);
	assert.equal(isCatalogEvidenceFresh(snapshotAt(new Date(now + 5 * 60_000).toISOString()), now), true);
});
