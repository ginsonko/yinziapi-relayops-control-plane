import type { TargetSite } from '../api/types';

function isLoopbackTarget(target: TargetSite) {
	try {
		const hostname = new URL(target.base_url).hostname.toLowerCase();
		return ['localhost', '127.0.0.1', '::1'].includes(hostname);
	} catch {
		return false;
	}
}

export function preferredEnabledTarget(targets: TargetSite[], preferredId?: string | number) {
	const enabled = targets.filter((target) => target.enabled);
	const explicit = preferredId === undefined || preferredId === ''
		? undefined
		: enabled.find((target) => String(target.id) === String(preferredId));
	return explicit ?? enabled.find((target) => !isLoopbackTarget(target)) ?? enabled[0];
}
