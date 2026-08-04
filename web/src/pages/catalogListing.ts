import type { CatalogSnapshot } from '../api/types';

export const catalogFreshnessWindowMillis = 10 * 60_000;
export const catalogFutureToleranceMillis = 5 * 60_000;

export function isCatalogEvidenceFresh(snapshot?: CatalogSnapshot | null, now = Date.now()) {
	if (!snapshot?.evidence?.observed_at) return false;
	const observedAt = new Date(snapshot.evidence.observed_at).getTime();
	if (!Number.isFinite(observedAt) || observedAt > now + catalogFutureToleranceMillis) return false;
	return now - observedAt <= catalogFreshnessWindowMillis;
}
