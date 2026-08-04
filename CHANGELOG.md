# Changelog

All notable changes to RelayOps will be documented in this file. The project follows [Semantic Versioning](https://semver.org/) once the first beta tag is published.

## Unreleased

### Added

- Initial public source snapshot of the RelayOps control plane.
- Native NewAPI and Sub2API source connectors.
- Connector Manifest and declarative JSON Connector extension paths.
- NewAPI target adapter, controlled Option/channel writes, and optional group-model pricing contract.
- Group mirror, price mapping, mapping drafts, one-click listing, operations intelligence, watchlist, and wallboard.
- Chromium login pairing helper source and contract tests.
- Safe public defaults: no fixed administrator key; background workers, remote writes, and automation disabled by default.

### Known limitations

- Sub2API target-side writes are not built in.
- Modified NewAPI forks may require a Manifest, JSON Connector, or native adapter.
- Group-scoped model pricing requires the documented optional NewAPI control-plane extension.
- This beta does not claim universal compatibility with every upstream fork or billing scheme.
