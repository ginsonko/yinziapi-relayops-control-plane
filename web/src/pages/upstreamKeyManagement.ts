export type UpstreamKeyManagementSupplier = {
	base_url: string;
	adapter: string;
	metadata?: Record<string, string> | null;
};

export type UpstreamKeyManagementOption = {
	url: string;
	label: string;
	exact: boolean;
};

export type UpstreamKeyManagementLinks = {
	options: UpstreamKeyManagementOption[];
	homeUrl: string;
	configured: boolean;
};

const unsafeDecodedPathCharacter = /[\\/?#\u0000-\u001f\u007f]/;

function encodeCanonicalPathSegment(segment: string) {
	return encodeURIComponent(segment)
		.replace(/%3A/gi, ':')
		.replace(/%40/gi, '@')
		.replace(/%26/gi, '&')
		.replace(/%3D/gi, '=')
		.replace(/%2B/gi, '+')
		.replace(/%24/gi, '$')
		.replace(/%2C/gi, ',')
		.replace(/%3B/gi, ';');
}

export function normalizeUpstreamKeyManagementPath(rawPath: string): string | null {
	const path = rawPath.trim();
	if (!path) return '';
	if (new TextEncoder().encode(path).length > 512 || !path.startsWith('/') || path.startsWith('//') || /[\\?#\u0000-\u001f\u007f]/.test(path)) return null;
	if (path === '/') return '/';
	const trailingSlash = path.length > 1 && path.endsWith('/');
	const sourceSegments = path.split('/');
	const normalizedSegments: string[] = [];
	try {
		for (let index = 1; index < sourceSegments.length; index += 1) {
			const sourceSegment = sourceSegments[index];
			if (sourceSegment === '') {
				if (index === sourceSegments.length - 1 && trailingSlash) continue;
				return null;
			}
			const decodedSegment = decodeURIComponent(sourceSegment);
			if (!decodedSegment || decodedSegment === '.' || decodedSegment === '..' || unsafeDecodedPathCharacter.test(decodedSegment)) return null;
			let recursivelyDecoded = decodedSegment;
			while (/%[0-9a-f]{2}/i.test(recursivelyDecoded)) {
				recursivelyDecoded = decodeURIComponent(recursivelyDecoded);
				if (recursivelyDecoded === '.' || recursivelyDecoded === '..' || unsafeDecodedPathCharacter.test(recursivelyDecoded)) return null;
			}
			normalizedSegments.push(encodeCanonicalPathSegment(decodedSegment));
		}
	} catch {
		return null;
	}
	const normalizedPath = `/${normalizedSegments.join('/')}${trailingSlash && normalizedSegments.length > 0 ? '/' : ''}`;
	return normalizedPath;
}

export function upstreamKeyManagementLinks(supplier: UpstreamKeyManagementSupplier | null): UpstreamKeyManagementLinks | null {
	if (!supplier) return null;
	try {
		const base = new URL(supplier.base_url);
		if (!['http:', 'https:'].includes(base.protocol) || base.username || base.password) return null;
		const homeUrl = `${base.origin}/`;
		const configuredPath = supplier.metadata?.api_key_management_path?.trim() ?? '';
		if (configuredPath) {
			const normalizedPath = normalizeUpstreamKeyManagementPath(configuredPath);
			if (!normalizedPath) return { options: [], homeUrl, configured: false };
			const destination = new URL(normalizedPath, homeUrl);
			if (destination.origin === base.origin && !destination.username && !destination.password) {
				return { options: [{ url: destination.toString(), label: '打开已配置的 Key 管理页', exact: true }], homeUrl, configured: true };
			}
			return { options: [], homeUrl, configured: false };
		}
		const adapter = supplier.adapter.trim().toLowerCase();
		if (adapter === 'newapi') {
			return {
				options: [
					{ url: new URL('/keys', homeUrl).toString(), label: '新版 Key 页面', exact: false },
					{ url: new URL('/console/token', homeUrl).toString(), label: '经典版 Token 页面', exact: false },
				],
				homeUrl,
				configured: false,
			};
		}
		if (['sub2api', 'sub2api_upstream', 'sub2api_admin'].includes(adapter)) {
			return {
				options: [{ url: new URL('/keys', homeUrl).toString(), label: '打开 Sub2 Key 页面', exact: false }],
				homeUrl,
				configured: false,
			};
		}
		return { options: [], homeUrl, configured: false };
	} catch {
		return null;
	}
}
