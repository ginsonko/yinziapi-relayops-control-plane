export type ListingIdentitySubject = 'supplier' | 'group' | 'snapshot';

export type ListingIdentityFailureStatus =
	| 'invalid_identifier'
	| 'missing_identifier'
	| 'not_found'
	| 'ownership_mismatch'
	| 'ambiguous';

export type ListingIdentityResolution<T> =
	| {
		status: 'resolved';
		value: T;
		subject: ListingIdentitySubject;
		matchCount: 1;
		resolution: 'canonical' | 'exact' | 'legacy_compat' | 'ownership' | 'effect';
	}
	| {
		status: ListingIdentityFailureStatus;
		subject: ListingIdentitySubject;
		subjectLabel: string;
		matchCount: number;
	};

export interface CanonicalSupplierID {
	canonical: string;
	value: number;
}

export interface SupplierIdentityRecord {
	id: unknown;
	name?: unknown;
}

export interface CatalogGroupIdentityRecord {
	external_id: unknown;
	name?: unknown;
	model_ids?: readonly string[];
}

export interface SupplierIdentitySelection {
	supplierId?: unknown;
	supplierName?: unknown;
	requireExact?: boolean;
}

export interface CatalogGroupIdentitySelection {
	groupExternalId?: unknown;
	groupName?: unknown;
	requireExact?: boolean;
}

export interface ListingSourceEvidence {
	sourceSnapshotHash?: string;
	sourceObservedAt?: string;
	sourceEffectiveRatio?: string;
}

export type ResolvedListingSourceInput<
	S extends SupplierIdentityRecord,
	G extends CatalogGroupIdentityRecord,
> =
	| {
		sourceKind: 'catalog_group';
		supplier: S;
		group: G;
		evidence: ListingSourceEvidence & {
			sourceSnapshotHash: string;
			sourceEffectiveRatio: string;
		};
	}
	| {
		sourceKind: 'group';
		supplier: S;
		group: G;
		evidence?: ListingSourceEvidence;
	};

export interface ResolvedListingSource<
	S extends SupplierIdentityRecord = SupplierIdentityRecord,
	G extends CatalogGroupIdentityRecord = CatalogGroupIdentityRecord,
> {
	readonly sourceKind: 'group' | 'catalog_group';
	readonly supplier: S;
	readonly group: G;
	readonly supplierId: number;
	readonly supplierIdCanonical: string;
	readonly groupExternalId: string;
	readonly supplierName: string;
	readonly groupName: string;
	readonly modelIds: readonly string[];
	readonly evidence: Readonly<ListingSourceEvidence>;
}

export interface ListingSourceRenderProjection {
	readonly supplierId: number;
	readonly supplierName: string;
	readonly groupExternalId: string;
	readonly groupName: string;
	readonly modelIds: readonly string[];
}

export interface ListingPreviewIdentityProjection {
	readonly supplier_id: number;
	readonly discovery_external_id: string;
}

const canonicalSupplierIDPattern = /^[1-9][0-9]*$/;

function failure(
	status: ListingIdentityFailureStatus,
	subject: ListingIdentitySubject,
	matchCount = 0,
): ListingIdentityResolution<never> {
	return {
		status,
		subject,
		subjectLabel: subject === 'supplier' ? '上游站点' : subject === 'group' ? '上游分组' : '目录快照',
		matchCount,
	};
}

function isIdentifierPresent(value: unknown): boolean {
	return value !== undefined && value !== null;
}

function isGoTrimSpaceRune(codePoint: number): boolean {
	return (codePoint >= 0x0009 && codePoint <= 0x000d) ||
		codePoint === 0x0020 || codePoint === 0x0085 || codePoint === 0x00a0 || codePoint === 0x1680 ||
		(codePoint >= 0x2000 && codePoint <= 0x200a) ||
		codePoint === 0x2028 || codePoint === 0x2029 || codePoint === 0x202f || codePoint === 0x205f || codePoint === 0x3000;
}

export function parseCanonicalGroupToken(value: unknown): ListingIdentityResolution<string> {
	if (!isIdentifierPresent(value) || value === '') return failure('missing_identifier', 'group');
	if (typeof value !== 'string') return failure('invalid_identifier', 'group');
	const codePoints = Array.from(value, (part) => part.codePointAt(0)!);
	if (isGoTrimSpaceRune(codePoints[0]) || isGoTrimSpaceRune(codePoints[codePoints.length - 1])) {
		return failure('invalid_identifier', 'group');
	}
	return { status: 'resolved', value, subject: 'group', matchCount: 1, resolution: 'canonical' };
}

export function parseCanonicalSupplierID(input: unknown): ListingIdentityResolution<CanonicalSupplierID> {
	if (!isIdentifierPresent(input) || input === '') return failure('missing_identifier', 'supplier');
	if (typeof input === 'number') {
		if (!Number.isSafeInteger(input) || input <= 0) return failure('invalid_identifier', 'supplier');
		return {
			status: 'resolved',
			value: { canonical: String(input), value: input },
			subject: 'supplier',
			matchCount: 1,
			resolution: 'canonical',
		};
	}
	if (typeof input !== 'string' || !canonicalSupplierIDPattern.test(input)) {
		return failure('invalid_identifier', 'supplier');
	}
	const value = Number(input);
	if (!Number.isSafeInteger(value) || value <= 0 || String(value) !== input) {
		return failure('invalid_identifier', 'supplier');
	}
	return {
		status: 'resolved',
		value: { canonical: input, value },
		subject: 'supplier',
		matchCount: 1,
		resolution: 'canonical',
	};
}

export function resolveSupplierIdentity<T extends SupplierIdentityRecord>(
	candidates: readonly T[],
	selection: SupplierIdentitySelection,
): ListingIdentityResolution<T> {
	const hasExplicitID = isIdentifierPresent(selection.supplierId);
	if (selection.requireExact || hasExplicitID) {
		const requested = parseCanonicalSupplierID(selection.supplierId);
		if (requested.status !== 'resolved') return requested;
		const matches = candidates.filter((candidate) => {
			const candidateID = parseCanonicalSupplierID(candidate.id);
			return candidateID.status === 'resolved' && candidateID.value.canonical === requested.value.canonical;
		});
		if (matches.length === 0) return failure('not_found', 'supplier');
		if (matches.length !== 1) return failure('ambiguous', 'supplier', matches.length);
		return { status: 'resolved', value: matches[0], subject: 'supplier', matchCount: 1, resolution: 'exact' };
	}
	if (typeof selection.supplierName !== 'string' || selection.supplierName === '') {
		return failure('missing_identifier', 'supplier');
	}
	const matches = candidates.filter((candidate) => candidate.name === selection.supplierName);
	if (matches.length === 0) return failure('not_found', 'supplier');
	if (matches.length !== 1) return failure('ambiguous', 'supplier', matches.length);
	return { status: 'resolved', value: matches[0], subject: 'supplier', matchCount: 1, resolution: 'legacy_compat' };
}

export function resolveCatalogGroupIdentity<T extends CatalogGroupIdentityRecord>(
	candidates: readonly T[],
	selection: CatalogGroupIdentitySelection,
): ListingIdentityResolution<T> {
	const hasExplicitID = Object.prototype.hasOwnProperty.call(selection, 'groupExternalId');
	if (selection.requireExact || hasExplicitID) {
		const requested = parseCanonicalGroupToken(selection.groupExternalId);
		if (requested.status !== 'resolved') return requested;
		const matches = candidates.filter((candidate) => candidate.external_id === requested.value);
		if (matches.length === 0) return failure('not_found', 'group');
		if (matches.length !== 1) return failure('ambiguous', 'group', matches.length);
		return { status: 'resolved', value: matches[0], subject: 'group', matchCount: 1, resolution: 'exact' };
	}
	const requested = parseCanonicalGroupToken(selection.groupName);
	if (requested.status !== 'resolved') return requested;
	// Filtering once by array slot makes a row matching both predicates count once,
	// while distinct duplicate rows remain distinct and therefore ambiguous.
	const matches = candidates.filter((candidate) =>
		candidate.external_id === requested.value || candidate.name === requested.value,
	);
	if (matches.length === 0) return failure('not_found', 'group');
	if (matches.length !== 1) return failure('ambiguous', 'group', matches.length);
	const matchedExternalID = parseCanonicalGroupToken(matches[0].external_id);
	if (matchedExternalID.status !== 'resolved') return matchedExternalID;
	return { status: 'resolved', value: matches[0], subject: 'group', matchCount: 1, resolution: 'legacy_compat' };
}

export function verifyCatalogSnapshotOwnership<T extends { supplier_id: unknown }>(
	snapshot: T,
	expectedSupplierID: unknown,
): ListingIdentityResolution<T> {
	const expected = parseCanonicalSupplierID(expectedSupplierID);
	if (expected.status !== 'resolved') return expected;
	const actual = parseCanonicalSupplierID(snapshot.supplier_id);
	if (actual.status !== 'resolved' || actual.value.canonical !== expected.value.canonical) {
		return failure('ownership_mismatch', 'snapshot');
	}
	return { status: 'resolved', value: snapshot, subject: 'snapshot', matchCount: 1, resolution: 'ownership' };
}

export function normalizeResolvedListingSource<
	S extends SupplierIdentityRecord,
	G extends CatalogGroupIdentityRecord,
>(input: ResolvedListingSourceInput<S, G>): ListingIdentityResolution<ResolvedListingSource<S, G>> {
	const supplierID = parseCanonicalSupplierID(input.supplier.id);
	if (supplierID.status !== 'resolved') return supplierID;
	const groupID = parseCanonicalGroupToken(input.group.external_id);
	if (groupID.status !== 'resolved') return groupID;
	if (input.sourceKind === 'catalog_group') {
		if (!input.evidence.sourceSnapshotHash || !input.evidence.sourceEffectiveRatio) {
			return failure('missing_identifier', 'snapshot');
		}
	}
	const modelIds = Object.freeze([...(input.group.model_ids ?? [])]);
	const evidence = Object.freeze({ ...(input.evidence ?? {}) });
	const resolved = Object.freeze({
		sourceKind: input.sourceKind,
		supplier: input.supplier,
		group: input.group,
		supplierId: supplierID.value.value,
		supplierIdCanonical: supplierID.value.canonical,
		groupExternalId: groupID.value,
		supplierName: typeof input.supplier.name === 'string' && input.supplier.name ? input.supplier.name : `#${supplierID.value.canonical}`,
		groupName: typeof input.group.name === 'string' && input.group.name ? input.group.name : groupID.value,
		modelIds,
		evidence,
	}) satisfies ResolvedListingSource<S, G>;
	return { status: 'resolved', value: resolved, subject: 'group', matchCount: 1, resolution: 'exact' };
}

export function projectListingSourceForRender(
	resolved: ResolvedListingSource,
): ListingSourceRenderProjection {
	return Object.freeze({
		supplierId: resolved.supplierId,
		supplierName: resolved.supplierName,
		groupExternalId: resolved.groupExternalId,
		groupName: resolved.groupName,
		modelIds: resolved.modelIds,
	});
}

export function projectListingPreviewIdentity(
	resolved: ResolvedListingSource,
): ListingPreviewIdentityProjection {
	return Object.freeze({
		supplier_id: resolved.supplierId,
		discovery_external_id: resolved.groupExternalId,
	});
}

export async function runAfterCanonicalSupplierID<T>(
	input: unknown,
	effect: (supplierID: number) => T | Promise<T>,
): Promise<ListingIdentityResolution<T>> {
	const parsed = parseCanonicalSupplierID(input);
	if (parsed.status !== 'resolved') return parsed;
	const value = await effect(parsed.value.value);
	return { status: 'resolved', value, subject: 'supplier', matchCount: 1, resolution: 'effect' };
}

export async function runAfterCanonicalGroupToken<T>(
	input: unknown,
	effect: (groupToken: string) => T | Promise<T>,
): Promise<ListingIdentityResolution<T>> {
	const parsed = parseCanonicalGroupToken(input);
	if (parsed.status !== 'resolved') return parsed;
	const value = await effect(parsed.value);
	return { status: 'resolved', value, subject: 'group', matchCount: 1, resolution: 'effect' };
}
