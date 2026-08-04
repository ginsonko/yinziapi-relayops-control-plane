## What changed

Describe the behavior and protocol boundary changed by this pull request.

## Compatibility

State which NewAPI/Sub2API family, role, endpoint contract, billing unit, and fallback behavior were verified. Do not claim unsupported target-write capabilities.

## Evidence

- [ ] Sanitized fixture and contract tests added or updated
- [ ] `go test ./... -count=1`
- [ ] `go vet ./...`
- [ ] `pnpm --dir web build`
- [ ] `node --test tests/*.test.mjs`
- [ ] No production credentials, responses, databases, or deployment artifacts
- [ ] Remote-write changes include preview, revision/CAS, readback, and conditional rollback evidence

## Risk and rollback

Explain failure behavior, resource bounds, and how the change can be disabled or rolled back without affecting the API data plane.
