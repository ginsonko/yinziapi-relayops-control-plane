export const OVERVIEW_PREFERENCE_STORAGE_KEY = 'relayops_overview_preferences_v1';

export const OVERVIEW_METRIC_IDS = [
	'user_balance',
	'today_user_spend',
	'upstream_balance',
	'upstream_daily_spend',
	'today_gross_profit',
	'today_gross_margin',
	'operating_gap_estimate',
	'operating_gap_margin',
] as const;

export type OverviewMetricId = (typeof OVERVIEW_METRIC_IDS)[number];
export type OverviewViewMode = 'compact' | 'detailed';
export type OverviewActivityFilter = 'all' | 'attention';

export interface OverviewPreferences {
	version: 2;
	mode: OverviewViewMode;
	selectedMetricIds: OverviewMetricId[];
	hideUnavailable: boolean;
	activityFilter: OverviewActivityFilter;
}

const metricIdSet = new Set<string>(OVERVIEW_METRIC_IDS);

export function defaultOverviewPreferences(): OverviewPreferences {
	return {
		version: 2,
		mode: 'compact',
		selectedMetricIds: [...OVERVIEW_METRIC_IDS],
		hideUnavailable: true,
		activityFilter: 'all',
	};
}

export function normalizeOverviewPreferences(value: unknown): OverviewPreferences {
	if (!value || typeof value !== 'object' || Array.isArray(value)) return defaultOverviewPreferences();
	const candidate = value as Record<string, unknown>;
	if (candidate.version !== 1 && candidate.version !== 2) return defaultOverviewPreferences();

	const mode: OverviewViewMode = candidate.mode === 'detailed' ? 'detailed' : 'compact';
	const selectedMetricIds = Array.isArray(candidate.selectedMetricIds)
		? [...new Set(candidate.selectedMetricIds.filter((item): item is OverviewMetricId => typeof item === 'string' && metricIdSet.has(item)))]
		: [...OVERVIEW_METRIC_IDS];

	return {
		version: 2,
		mode,
		selectedMetricIds,
		hideUnavailable: typeof candidate.hideUnavailable === 'boolean' ? candidate.hideUnavailable : true,
		activityFilter: candidate.activityFilter === 'attention' ? 'attention' : 'all',
	};
}

export function loadOverviewPreferences(): OverviewPreferences {
	if (typeof window === 'undefined') return defaultOverviewPreferences();
	try {
		const stored = window.localStorage.getItem(OVERVIEW_PREFERENCE_STORAGE_KEY);
		return stored ? normalizeOverviewPreferences(JSON.parse(stored)) : defaultOverviewPreferences();
	} catch {
		return defaultOverviewPreferences();
	}
}

export function saveOverviewPreferences(preferences: OverviewPreferences) {
	if (typeof window === 'undefined') return;
	try {
		window.localStorage.setItem(OVERVIEW_PREFERENCE_STORAGE_KEY, JSON.stringify(preferences));
	} catch {
		// Storage can be unavailable in hardened/private browser contexts.
	}
}
