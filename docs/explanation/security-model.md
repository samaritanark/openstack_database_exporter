# Security model

What `openstack_database_exporter` protects, from whom, and which mechanism is responsible for each guarantee. Each control is mapped to where it's implemented and how it's tested. Everything the exporter permits on purpose, leaves to operators, or accepts as a risk has an ID in [Security allowances and residual risks](../reference/security-allowances.md).

The exporter reads the databases through the [openstackdb](https://github.com/vexxhost/openstackdb) library. Query-level controls, such as the read-only query set and the absence of secret columns, belong to that library and are described in the library's own security model, `docs/explanation/security-model.md` in the [openstackdb repository](https://github.com/vexxhost/openstackdb) (added in openstackdb after v0.2.1). This page lists which of them the exporter relies on, and doesn't repeat them.

## The short version

The exporter is a long-running service. It holds a database credential for each configured OpenStack service, runs a fixed set of read-only queries on every scrape, and serves the results over HTTP. Everything on this page follows from that:

- **Whoever can scrape sees everything.** Metrics carry every tenant's project names, resource names, IDs, and addresses. The listener has no authentication or TLS unless you configure them (AL-1, AL-2).
- **Scrapes carry nothing into SQL.** An HTTP request triggers queries but supplies no values to them. Every query is a parameterless list call from openstackdb.
- **Connection URLs are secrets.** Pass them as environment variables, not flags (AL-3). The exporter never writes them to logs or metric output.
- **Read-only is enforced by your database grants.** The query set is read-only by construction, but only a `SELECT`-only account makes the connection read-only (EX-1). The grants each collector needs are listed in [Database grants](#database-grants).
- **Load controls are thin.** Each database pool is bounded, but queries have no deadline and scrapes have no concurrency limit (R-1).

## Assets

| Asset | Why it matters |
| --- | --- |
| Database credentials | Up to 12 connection URLs, one per service database, Keystone included. Each usually grants access to a whole service database |
| Integrity and availability of the OpenStack databases | These are the live control-plane databases. The exporter shares them with the services, and every scrape loads them |
| Tenant inventory in the metric output | Project names, descriptions, and tags, server, volume, image, and network names, user and project IDs, IP and MAC addresses, and hypervisor hostnames |
| Secret columns next to the tables the exporter reads | For example Nova `instances.user_data`; Ironic `nodes.driver_info`, `nodes.instance_info`, and `nodes.driver_internal_info`; Cinder `volumes.provider_auth`, `snapshots.provider_auth`, and `volume_attachment.connection_info`; and Magnum `cluster.trustee_password`. The exporter never selects them, but a leaked credential with table-level grants can |
| The published container image | Operators run it with database credentials in its environment |

## Trust boundaries

| Input | Trust | Reasoning |
| --- | --- | --- |
| Connection URLs (`--<service>.database-url` flags and `<SERVICE>_DATABASE_URL` variables) | Trusted operator configuration, secret | Whoever sets them already holds the credential |
| `--web.config.file` and the other `--web.*` flags | Trusted operator configuration | They decide who can reach the metrics |
| HTTP requests to the listener | **Untrusted** | Anyone who can reach the port can send them. They start a scrape but supply no query values |
| Database contents used as label values | **Untrusted text** | Tenants choose resource names, descriptions, and tags |
| Database contents used as sample values | Trusted as data | Counts, sizes, and states. They're never interpreted as code or SQL |
| The openstackdb module and other Go modules | Trusted, pinned | `go.mod` and `go.sum` pin every version and checksum |
| Base image and GitHub Actions | Trusted, pinned | Pinned by digest and by commit SHA |

## Threat model

### Anyone who can reach the listener

The listener binds to `:9180` on every interface by default and serves plain HTTP with no authentication (AL-1).

- **Reading tenant data.** Every scrape returns the full cross-tenant inventory (AL-2). The exporter makes no per-tenant decisions; protect the endpoint and every system that stores the metrics (EX-2, EX-5).
- **Loading the databases.** Every scrape runs every configured collector's queries. Queries run with no deadline, and the handler has no concurrency limit, so concurrent or abandoned scrapes keep queries running (R-1, R-2). Each pool caps open connections at 5. Keystone and Placement can have two pools each (the project resolver and the Keystone collector; the Nova and Placement collectors), so up to 10 connections to those databases. This bounds the load on each database but not the requests waiting on the exporter.
- **Controls.** `--web.config.file` enables TLS, basic authentication, and a request rate limit through the exporter-toolkit. `--web.listen-address` narrows the bind address.

### Tenant of the cloud

A tenant controls resource names, descriptions, and tags, and these become label values.

- **Injection into SQL.** None of these values reach a query. Every query is parameterless.
- **Injection into the exposition format.** The client_golang text encoder escapes quotes, backslashes, and newlines in label values, so a label value can't add series or labels.
- **Injection into consumers.** Dashboards, alert templates, and notification receivers that render label values treat them as untrusted (EX-7).
- **Crashing the exporter.** A label value that isn't valid UTF-8 makes `prometheus.MustNewConstMetric` panic, which ends the process (R-4).
- **Series growth.** The number of series grows with the number of tenant resources (R-5).

### Local user on the exporter's host

Connection URLs passed as flags appear in the process list (AL-3). Environment variables are readable by the same user and by root. Pass URLs only through environment variables from a secret store, and run the exporter as a dedicated non-root user (EX-4, EX-6).

### Network attacker between the exporter and a database

The exporter doesn't configure TLS to the databases. Unless the connection URL sets `tls=…`, the login handshake and every result cross the network in plaintext (EX-3; openstackdb AL-2 and AL-3).

### Compromised exporter process or image

The process holds every configured credential (PR-1). Only the database grants limit what an attacker can then read or change (EX-1). Table-level grants also expose the secret columns that share a table with the data the exporter reads (R-6).

### Supply chain

Go modules are pinned in `go.sum`, actions by commit SHA, and the builder image by digest. Renovate proposes updates, with its behavior set by a shared preset outside this repository (R-13). The runner hardening only audits egress (R-7), CI runs no Go vulnerability scan (R-8), and the nightly scan may not cover the published image (R-10). Images are built on a third-party remote builder (R-11) and aren't signed (R-12).

## The controls

### Owned by the exporter

| Control | Implementation | Tests |
| --- | --- | --- |
| TLS, basic authentication, and rate limiting available on the listener | `webflag.AddFlags` (`cmd/openstack-database-exporter/main.go:23`) and `web.ListenAndServe` (`main.go:128`) from exporter-toolkit, configured by `--web.config.file` | exporter-toolkit's own tests; none in this repository |
| Invalid web configuration stops the process | `web.ListenAndServe` error exits with status 1 (`main.go:128-131`) | None in this repository |
| No request data reaches SQL | Each `Collect` takes only the metric channel and calls parameterless list methods, for example `ImageGetAll(ctx)` (`internal/collector/glance/images.go:81`) | Unit tests match the library's exact SQL with go-sqlmock, for example `regexp.QuoteMeta(glancedb.ImageGetAll)` in `internal/collector/glance/images_test.go` |
| No SQL of its own | Database access only through openstackdb query types, such as `glancedb.New` and `octaviadb.NewAmphoraRepository` | Security review. A search for `Query`, `QueryRow`, and `Exec` calls in `cmd/` and `internal/` finds them only in `_test.go` files and `internal/testutil` |
| Connection URLs not logged | Registration logs only the error returned by `db.Connect`, for example `internal/collector/glance/glance.go:22-24`, `internal/collector/nova/nova.go:24-43`, and `internal/collector/collector.go:53-55` | openstackdb `TestMalformedURLDoesNotExposeCredentials`; none in this repository |
| An unconfigured service opens no connection | `RegisterCollectors` returns before `db.Connect` when its URL is empty, for example `internal/collector/glance/glance.go:17-20` | `TestNewRegistry_AllEmpty` |
| Failures stay within one service | `util.NewDownCollector` when `Connect` fails (`internal/util/down.go`). A query error affects only that service's series. Most collectors also report `_up 0`, for example `internal/collector/glance/images.go:81-87` | `TestDownCollector`, the "query error" cases with `WillReturnError`, and `TestIntegration_E2E_DBDownResilience` |
| Data race detection | `-race` in the unit and integration test jobs (`.github/workflows/ci.yaml:43`, `:58`) | `TestIntegration_E2E_ConcurrentScrapes` |
| Minimal runtime image | `FROM scratch` with only the CA bundle and a static binary (`Dockerfile:9`, `:11-13`) | Image build in `.github/workflows/image.yaml` |
| Pinned build inputs | Builder image by digest (`Dockerfile:4`), modules in `go.sum`, every action by commit SHA, updates through Renovate (`renovate.json`) | Review of Renovate pull requests |
| Runner hardening | `step-security/harden-runner` as the first step of every job, in `audit` mode | None |
| Registry login and image push skipped for pull requests | Workflow-level `contents: read`; `packages: write` and `id-token: write` only on the image job (`image.yaml:11-20`); registry login and push skipped for pull requests (`image.yaml:33`, `:42`) | None |

### Inherited from dependencies

| Control | Owner | Exporter's part |
| --- | --- | --- |
| Read-only query set, with no writes, locks, or DDL | openstackdb | Calls only list methods |
| No secret columns selected | openstackdb | Adds no queries. Labels come only from the selected columns |
| Connection URL not echoed in errors | openstackdb `ParseOsloDBConnectionString` | Logs the error, never the URL |
| Bounded pool of 5 open and 2 idle connections, 10-second connect deadline | openstackdb `Connect` | Uses `db.Connect` for every database. Each pool caps open connections at 5 (10 for Keystone and Placement, which have two pools each) |
| Label value escaping in the text format | client_golang `expfmt` | Builds metrics only with `prometheus.MustNewConstMetric` |
| TLS, basic authentication with bcrypt hashes, rate limiting | exporter-toolkit `web` | Passes the flags through |

When you bump openstackdb, check its allowances register for changes, and update [Database grants](#database-grants) if the queries read new tables.

## Database grants

Give the exporter its own account with `SELECT` only, never the service's own account (EX-1). The tables below are what each collector reads through openstackdb v0.2.1. Database names vary by deployment.

| Service | Flag | Tables |
| --- | --- | --- |
| Cinder | `--cinder.database-url` | `quota_usages`, `quotas`, `services`, `snapshots`, `volume_attachment`, `volume_types`, `volumes` |
| Glance | `--glance.database-url` | `images` |
| Heat | `--heat.database-url` | `stack` |
| Ironic | `--ironic.database-url` | `nodes` |
| Keystone | `--keystone.database-url` | `group`, `project`, `project_tag`, `region`, `user` |
| Magnum | `--magnum.database-url` | `cluster`, `nodegroup` |
| Manila | `--manila.database-url` | `availability_zones`, `share_instances`, `share_types`, `shares` |
| Neutron | `--neutron.database-url` | `agents`, `dnsnameservers`, `externalnetworks`, `floatingips`, `ha_router_agent_port_bindings`, `ipallocationpools`, `ipallocations`, `ml2_port_bindings`, `networkrbacs`, `networks`, `networksegments`, `ports`, `quotas`, `routers`, `securitygrouprules`, `securitygroups`, `standardattributes`, `subnetpoolprefixes`, `subnetpools`, `subnets`, `tags` |
| Nova (cell database) | `--nova.database-url` | `compute_nodes`, `instances`, `services` |
| Nova API | `--nova-api.database-url` | `aggregate_hosts`, `aggregates`, `flavors`, `quota_classes`, `quotas` |
| Octavia | `--octavia.database-url` | `amphora`, `load_balancer`, `pool`, `vip` |
| Placement | `--placement.database-url` | `allocations`, `consumers`, `inventories`, `projects`, `resource_classes`, `resource_providers` |

The Nova collector registers only when both Nova URLs are set, and it also reads Placement when that URL is set. The Keystone account also serves the project name resolver.

Some of these tables hold secret columns that the exporter never selects, for example `instances.user_data`, `nodes.driver_info`, `nodes.instance_info`, `nodes.driver_internal_info`, `volumes.provider_auth`, `snapshots.provider_auth`, `volume_attachment.connection_info`, and `cluster.trustee_password`. To keep them out of reach of a leaked credential, grant `SELECT` on the needed columns of those tables instead of on the whole table (R-6).

## Failure behavior

| Condition | Result |
| --- | --- |
| A service's URL is empty | No connection and no collector for that service |
| `Connect` fails at startup | `openstack_<subsystem>_up 0` until the process restarts |
| A query fails during a scrape | The error is logged. Most collectors set the service's `_up` gauge to 0; some, for example the Neutron network, subnet, and floating IP collectors, only drop their own series, so `_up` can stay 1. Other services are unaffected |
| Keystone is unreachable | Tenant labels fall back to project IDs (`internal/collector/project/resolver.go`) |
| `--web.config.file` is invalid | The process exits with status 1 |
| A label value isn't valid UTF-8 | The process panics (R-4) |

## Review history

| Date | Scope | Outcome |
| --- | --- | --- |
| 2026-11-20 | Partial baseline review of the whole repository while writing this model | Recorded outside the repository |
