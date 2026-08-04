import type { PageId } from '../components/Shell';
import type { BootstrapData } from './types';

export type BootstrapScope = 'full' | 'overview' | 'pricing' | 'changes' | 'connectors' | 'discoveries';

export function scopeForPage(page: PageId): BootstrapScope {
	switch (page) {
		case 'pricing': return 'pricing';
		case 'changes': return 'changes';
		case 'connectors': return 'connectors';
		case 'discoveries': return 'discoveries';
		default: return 'overview';
	}
}

export function mergeBootstrapData(current: BootstrapData | null, incoming: BootstrapData, scope: BootstrapScope): BootstrapData {
	if (!current || scope === 'full') return incoming;
	const merged: BootstrapData = {
		...current,
		automationEnabled: incoming.automationEnabled,
		backgroundWorkersEnabled: incoming.backgroundWorkersEnabled,
		remoteWritesEnabled: incoming.remoteWritesEnabled,
		metrics: incoming.metrics,
		suppliers: incoming.suppliers,
		watchItems: incoming.watchItems,
		watchSettings: incoming.watchSettings,
		watchCandidateCount: incoming.watchCandidateCount,
		balances: incoming.balances,
		runtime: incoming.runtime,
		generatedAt: incoming.generatedAt,
	};
	if (scope === 'pricing') merged.policies = incoming.policies;
	if (scope === 'changes') {
		merged.proposals = incoming.proposals;
		merged.activities = incoming.activities;
	}
	if (scope === 'connectors') {
		merged.connectors = incoming.connectors;
		merged.credentials = incoming.credentials;
	}
	if (scope === 'discoveries') merged.discoveries = incoming.discoveries;
	return merged;
}
