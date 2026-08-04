# Third-Party Notices

RelayOps source code is licensed under Apache License 2.0. It depends on third-party packages that retain their own licenses and copyright notices. The authoritative dependency versions are recorded in `go.sum` and `web/pnpm-lock.yaml`.

## Go dependencies

| Package family | License |
|---|---|
| `github.com/go-chi/chi/v5` | MIT |
| `github.com/shopspring/decimal` | MIT |
| `github.com/dustin/go-humanize` | MIT |
| `github.com/google/uuid` | BSD-3-Clause |
| `github.com/mattn/go-isatty` | MIT |
| `github.com/ncruces/go-strftime` | MIT |
| `github.com/remyoudompheng/bigfft` | BSD-3-Clause |
| `golang.org/x/exp`, `golang.org/x/sys` and related Go tooling modules | BSD-3-Clause |
| `modernc.org/sqlite` and its `modernc.org/*` support modules | BSD-3-Clause |

## Frontend dependencies

| Package family | License |
|---|---|
| React and React DOM | MIT |
| Lucide React | ISC |
| Rsbuild and its React plugin | MIT |
| TypeScript | Apache-2.0 |
| React type definitions | MIT |

Transitive packages are installed from the locked package manifests and are not vendored into this repository. Their license files are distributed by their respective packages.

## Other material

`CODE_OF_CONDUCT.md` is adapted from Contributor Covenant 2.0 and contains its required attribution. The browser extension uses Chromium extension APIs but bundles no Chromium source, fonts, screenshots, or third-party image assets.

New API, Sub2API, and upstream product names are used only to describe interoperability. Their trademarks and source licenses belong to their respective owners. RelayOps does not include or relicense their source code.
