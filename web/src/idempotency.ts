function randomSuffix() {
	const cryptoAPI = globalThis.crypto;
	if (typeof cryptoAPI?.randomUUID === 'function') return cryptoAPI.randomUUID();
	if (typeof cryptoAPI?.getRandomValues === 'function') {
		const values = new Uint32Array(4);
		cryptoAPI.getRandomValues(values);
		return Array.from(values, (value) => value.toString(36)).join('-');
	}
	return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}-${Math.random().toString(36).slice(2)}`;
}

export function newIdempotencyKey(prefix: string) {
	return `${prefix}-${randomSuffix()}`;
}
