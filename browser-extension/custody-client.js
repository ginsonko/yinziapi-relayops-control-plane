// Fixed-route client shared by the popup and service worker. Callers cannot
// turn a custody token into an arbitrary RelayOps request capability.
(function installRelayOpsCustody(global) {
  const ROUTES = Object.freeze({
    status: '/api/v1/browser-custody/status',
    capture: '/api/v1/browser-custody/capture',
    revoke: '/api/v1/browser-custody',
  });

  function isTailscaleIPv4(hostname) {
    const octets = hostname.split('.');
    if (octets.length !== 4) return false;
    const numbers = octets.map((part) => Number(part));
    if (numbers.some((part, index) => !Number.isInteger(part) || part < 0 || part > 255 || String(part) !== octets[index])) {
      return false;
    }
    return numbers[0] === 100 && numbers[1] >= 64 && numbers[1] <= 127;
  }

  function relayOpsOrigin(raw) {
    const parsed = new URL(String(raw || '').trim());
    if (!['http:', 'https:'].includes(parsed.protocol) || parsed.username || parsed.password) {
      throw new Error('RelayOps address must be an HTTP origin without embedded credentials');
    }
	const hostname = parsed.hostname.toLowerCase();
	const encryptedPrivateTransport = ['localhost', '127.0.0.1', '[::1]'].includes(hostname) || isTailscaleIPv4(hostname);
	if (parsed.protocol === 'http:' && !encryptedPrivateTransport) {
	  throw new Error('远程 RelayOps 必须使用 HTTPS；HTTP 只允许 localhost、127.0.0.1 或 [::1]');
	}
    return parsed.origin;
  }

  function routeURL(origin, route) {
    if (!Object.prototype.hasOwnProperty.call(ROUTES, route)) throw new Error('unsupported custody route');
    return `${relayOpsOrigin(origin)}${ROUTES[route]}`;
  }

  async function request(origin, route, token, options = {}) {
    if (!String(token || '').trim()) throw new Error('browser custody token is missing');
    const { timeoutMs: requestedTimeout, ...requestOptions } = options;
    const timeoutMs = Number.isFinite(requestedTimeout) ? Math.max(1000, requestedTimeout) : 45_000;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    const version = global.chrome?.runtime?.getManifest?.().version || '0.0.0';
    try {
      const response = await fetch(routeURL(origin, route), {
        ...requestOptions,
        cache: 'no-store',
        credentials: 'omit',
        signal: controller.signal,
        headers: {
          ...(requestOptions.headers || {}),
          'X-RelayOps-Custody': token,
          'X-RelayOps-Helper-Version': version,
        },
      });
      const result = await response.json().catch(() => null);
      return { response, result };
    } finally {
      clearTimeout(timer);
    }
  }

  global.RelayOpsCustody = { relayOpsOrigin, routeURL, request, routes: ROUTES };
})(globalThis);
