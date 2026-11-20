# AGENTS.md

Guidance for AI coding agents and contributors working in this repository. The human-facing overview is in `README.md`.

## Project

`github.com/vexxhost/openstack_database_exporter` is a long-running Prometheus exporter. It reads OpenStack service databases (MySQL or MariaDB) and serves the results as metrics over HTTP. All SQL comes from the [openstackdb](https://github.com/vexxhost/openstackdb) library, which is pinned in `go.mod`. This repository owns the collectors, the HTTP listener, the container image, and the metric-output tests.

## Layout

| Path | What it is |
| --- | --- |
| `cmd/openstack-database-exporter/main.go` | Flags, logging, the HTTP listener (exporter-toolkit), and the `/metrics` handler |
| `cmd/openstack-database-exporter/main_test.go` | End-to-end integration tests (`//go:build integration`) that run the full registry against MariaDB containers |
| `internal/collector/collector.go` | `Config` and `NewRegistry`, which wires every service and the shared project resolver |
| `internal/collector/<service>/` | One package per service: cinder, glance, heat, ironic, keystone, magnum, manila, neutron, nova, octavia, and placement. `<service>.go` holds `RegisterCollectors`; the other files hold one collector each |
| `internal/collector/<service>/integration_test.go` | Per-service integration tests (`//go:build integration`) |
| `internal/collector/project/` | Keystone project ID to name and domain resolver, cached for `--project-cache-ttl` |
| `internal/util/` | `NewDownCollector` (reports `_up 0` when a database can't be reached at startup) and shared helpers |
| `internal/testutil/` | `RunCollectorTests` (go-sqlmock table tests) and MariaDB testcontainers helpers (`NewMySQLContainer`, `NewMySQLContainerWithURL`, `SeedSQL`) |
| `Dockerfile` | Two-stage build: Go builder pinned by digest, `scratch` runtime image |
| `.github/workflows/` | `ci.yaml` (lint, unit, integration, build), `image.yaml` (multi-arch image build and push), `periodic.yaml` (nightly image scan) |
| `.ai/` | Tool-agnostic agent workflows (see below) |
| `local/` | Gitignored. Private notes and security review reports. Never committed |

## Commands

Run Go commands from the repository root. If you keep a `go.work` file outside the repository, set `GOWORK=off` so you build against the pinned modules.

```sh
go build -o /dev/null ./cmd/openstack-database-exporter
go vet ./...
go vet -tags integration ./...                                 # also type-checks the integration tests
gofmt -l .                                                     # must print nothing
go test -count=1 -short -race ./...                            # unit tests, as CI runs them
go test -count=1 -race -tags integration -timeout 15m ./...    # integration tests; needs Docker
golangci-lint run                                              # default linters; the repository has no config file
docker build .                                                 # container image
```

CI (`.github/workflows/ci.yaml`) runs golangci-lint, the unit tests, the integration tests, and the build on every pull request and every push to `main`. `image.yaml` builds the image for `linux/amd64` and `linux/arm64` and pushes it only for pushes to `main` and `v*` tags.

Integration tests pass schema paths such as `"../../../sql/nova/schema.sql"`. `internal/testutil` maps any path containing `sql/` onto the schema files embedded in the pinned openstackdb module, so there is no `sql/` directory on disk.

## Conventions

- **No SQL in this repository.** Query changes belong in openstackdb. Release them there, then bump the pinned version in `go.mod` here after you validate compatibility. Don't call `Query`, `QueryContext`, or `Exec` outside test code.
- **Read-only.** The exporter only reads. Never add a code path that writes to an OpenStack database.
- **Collector pattern.** `RegisterCollectors` returns without connecting when its database URL is empty. When `Connect` fails, it registers `util.NewDownCollector`. When a query fails during a scrape, the collector logs the error and, where it owns the service's `_up` gauge, reports `openstack_<subsystem>_up 0`.
- **Metric names and labels are an interface.** Dashboards and alerts depend on them. Keep existing names stable, and follow the names that the OpenStack API exporter uses where one exists.
- **Label values come from the database.** Treat them as untrusted text. Don't add a label that carries a secret column, and check the label against [Security allowances and residual risks](docs/reference/security-allowances.md) (AL-2) before you add new tenant data.
- **Database grants.** When a collector reads a new table, update the grants table in [Security model](docs/explanation/security-model.md#database-grants).
- **Tests.** Unit tests use go-sqlmock through `testutil.RunCollectorTests` and compare the exposition text. Behavior against real SQL goes in `integration_test.go` files with the `integration` build tag.
- **Formatting and commits.** Run `gofmt`. Commit subjects use Conventional Commits prefixes such as `feat:`, `fix:`, `refactor:`, and `chore(deps):`.
- **Docs.** Update `README.md` when flags or exported metrics change.

## Security

The exporter is an operator-facing network service. Whoever can scrape it sees every tenant's inventory, and its process holds a database credential for every configured service.

- Database connection URLs are trusted operator configuration, but they hold credentials. Never put one, or the DSN built from it, into a log, an error, a metric, a test name, or a commit message. Test fixtures use the `test:test` credentials in `internal/testutil` only.
- Database contents are untrusted text when they become label values.
- `docs/explanation/security-model.md`: assets, trust boundaries, threat model, each control mapped to its code and tests, and the database grants each collector needs.
- `docs/reference/security-allowances.md`: the register of deliberate allowances (AL), privileges (PR), controls owned outside the exporter (EX), residual risks (R), and fixed findings (SR). Open findings are never committed; they stay in `local/` until the fix ships. Check any security change against it.
- `.ai/context/security.md`: security policy for agents, including the actions that need explicit confirmation.
- `local/` is gitignored and must never be committed. Full security review reports live in `local/security-review/<date>/`.

## Agent workflows (`.ai/`)

Tool-agnostic instructions any agent can follow:

- `.ai/context/security.md`: security policy for agents.
- `.ai/security-review.md`: security review of the current branch's changes, or a baseline review of the whole repository. It reads `AGENTS.md`, the register, the security model, and its own instructions from the base ref, so a branch can't pre-allow its own findings. In Claude Code, `/security-review` runs this project skill (`.claude/skills/security-review/`), which replaces the built-in `/security-review` here.
- `.ai/security-review-false-positives.md`: rules for verifying review findings.

Tool-specific wrappers, such as `.claude/skills/`, point at these files rather than duplicate them.
