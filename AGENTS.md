# RelayOps repository instructions

These instructions apply to the entire repository when using Codex or another coding agent.

## Read first

Before changing protocol, pricing, storage, or remote-write behavior, read:

- `docs/ARCHITECTURE.md`
- `docs/ADAPTER_CONTRACTS.md`
- `docs/GROUP_MODEL_PRICING.md`
- `docs/CONNECTOR_DEVELOPMENT.md`

## Non-negotiable boundaries

- Never provide or commit real tokens, cookies, passwords, private keys, databases, full authenticated HAR files, or production response bodies.
- Work from sanitized fixtures. Replace domains, account IDs, balances, and model/customer identities while preserving the response structure and billing units.
- Do not connect tests to a production supplier or target. Do not enable remote writes for development.
- Keep Source Connectors read-only. A successful source connector does not prove any Target Adapter capability.
- Never guess a price, currency, billing mode, or duration unit. Unknown semantics must stop writes and retain the last trusted snapshot.
- Use decimal strings for prices and ratios; do not introduce floating-point comparisons into pricing paths.
- Remote writes require preview, expected revision/hash, fencing guard, apply, independent readback, receipt, and conditional rollback.
- Network, DNS, TLS, 429, and upstream 5xx failures must not erase trusted catalog state or create model-removal plans.
- Preserve zero prices as valid values. Do not confuse zero with missing evidence.
- Do not add generated frontend output, archives, binaries, logs, SQLite files, `.env`, or deployment credentials to Git.

## Extension order

For a modified upstream, choose the smallest correct implementation:

1. Connector Manifest for path, Header, login-field, or compatible JSON-location changes.
2. JSON Connector for a stable, declarative JSON protocol.
3. Native Go Source Connector only for semantics that cannot be represented declaratively.
4. A separate Target Adapter when RelayOps must write to a non-NewAPI control plane.

Write a failing contract test and sanitized fixture before implementation. Cover normal, empty, paginated, zero-price, 401/403, 429, 5xx, truncated, unknown-unit, and schema-drift cases as applicable.

## Required validation

```bash
go test ./... -count=1
go vet ./...
go build ./cmd/relayops
pnpm --dir web install --frozen-lockfile
pnpm --dir web build
node --test tests/*.test.mjs
```

Run `git diff --check` and inspect the exact staged file list before committing. If a live protocol check is unavoidable, use a disposable account/target, disable automation, and document the bounded read-only evidence. Production writes are outside normal contribution scope.
