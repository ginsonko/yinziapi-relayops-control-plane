import assert from 'node:assert/strict';
import test from 'node:test';
import {
	OVERVIEW_METRIC_IDS,
	defaultOverviewPreferences,
	normalizeOverviewPreferences,
} from '../src/pages/overviewPreferences.ts';

test('defaults to compact mode with every metric selected and unavailable metrics folded', () => {
	assert.deepEqual(defaultOverviewPreferences(), {
		version: 1,
		mode: 'compact',
		selectedMetricIds: [...OVERVIEW_METRIC_IDS],
		hideUnavailable: true,
	});
});

test('rejects unknown versions instead of inheriting stale preferences', () => {
	assert.deepEqual(normalizeOverviewPreferences({ version: 2, mode: 'detailed' }), defaultOverviewPreferences());
});

test('cleans duplicate and unknown metric ids while preserving an intentional empty selection', () => {
	assert.deepEqual(normalizeOverviewPreferences({
		version: 1,
		mode: 'detailed',
		selectedMetricIds: ['today_user_spend', 'invalid', 'today_user_spend'],
		hideUnavailable: false,
	}), {
		version: 1,
		mode: 'detailed',
		selectedMetricIds: ['today_user_spend'],
		hideUnavailable: false,
	});

	assert.deepEqual(normalizeOverviewPreferences({
		version: 1,
		mode: 'compact',
		selectedMetricIds: [],
		hideUnavailable: true,
	}).selectedMetricIds, []);
});

test('falls back field by field when a version-one payload is incomplete', () => {
	assert.deepEqual(normalizeOverviewPreferences({ version: 1, mode: 'other' }), defaultOverviewPreferences());
});
