# Security allowances and residual risks

A register of what `openstack_database_exporter` permits on purpose, what it leaves to operators, and the risks it accepts. It's written for security reviewers comparing findings against intended behavior. A finding that matches an entry here is known and deliberate, and the entry says why and what bounds it. A finding that contradicts an entry is a defect.

The reasoning behind each entry is in the [Security model](../explanation/security-model.md). Entries that belong to the openstackdb library, such as its AL-2 (no TLS unless the connection URL asks for it) and AL-3 (driver parameters pass through), are in the library's own register, `docs/reference/security-allowances.md` in the [openstackdb repository](https://github.com/vexxhost/openstackdb) (added in openstackdb after v0.2.1), and apply here too.

## Deliberate allowances

Behavior that may look like a weakness in a review but is intended.

| ID | Allowance | Why | What bounds it |
| --- | --- | --- | --- |
| AL-1 | **Open listener by default.** The exporter listens on `:9180` on every interface, over plain HTTP, with no authentication (`cmd/openstack-database-exporter/main.go:23`) | Prometheus exporter convention. Certificates, users, and bind addresses are site configuration | `--web.config.file` enables TLS, basic authentication, and rate limiting. `--web.listen-address` narrows the bind address. Network access control (EX-5) |
| AL-2 | **Cross-tenant inventory in labels.** Metrics carry project names, descriptions, and tags, resource names, user and project IDs, IP and MAC addresses, CIDRs, and hypervisor and agent hostnames for every tenant | The exporter is operator-facing. Its purpose is a full view of the cloud | No secret columns are selected (openstackdb). Access to the endpoint and to every system that stores the metrics (EX-2) |
| AL-3 | **Connection URLs accepted as flags.** Every `--<service>.database-url` flag also reads a `<SERVICE>_DATABASE_URL` environment variable (`main.go:26-73`). A value passed as a flag is part of the process command line, so treat it as readable outside the process. This entry covers accepting the flag only: any path that makes a URL readable through logs, errors, metrics, or HTTP responses is a defect, not part of AL-3 | Standard kingpin flag handling | Always pass URLs through environment variables from a secret store (EX-4) |
| AL-4 | **Database error text is logged.** Connect and query errors are logged as returned by the driver, which can include the database user name, host, and table names | Operators need the cause to fix grants and connectivity | The password and the URL never appear: openstackdb drops them from its errors, and the exporter logs only the error |

## Privileges the exporter holds

| ID | Holder | Holds | Note |
| --- | --- | --- | --- |
| PR-1 | The exporter process | One database account per configured service URL, up to 12, Keystone included | The exporter only runs `SELECT`s, but it can't narrow the accounts. A compromised process or image can use every credential directly. Only the grants (EX-1) bound it |
| PR-2 | The `image` workflow's build job | `packages: write` and `id-token: write` (`.github/workflows/image.yaml:17-20`) | Registry login and push run only outside pull requests (`image.yaml:33`, `:42`), but both permissions are still granted on pull request runs from branches in this repository. The workflow default is `contents: read` |
| PR-3 | The `periodic` workflow | `security-events: write` (`.github/workflows/periodic.yaml:7-9`) | Granted to the nightly image scan job. Runs only on a schedule |

## Owned outside the exporter

Controls the exporter depends on but doesn't implement. Each needs an owner at the site.

| ID | Control | Why it matters | Exporter's part |
| --- | --- | --- | --- |
| EX-1 | **A dedicated `SELECT`-only database account**, limited to the tables in [Database grants](../explanation/security-model.md#database-grants), never the service's own account | It's the only real read-only boundary, and it limits what a leaked credential exposes | Documents the tables each collector reads. Not checked at startup |
| EX-2 | **Access control for the metrics**: the endpoint, Prometheus, dashboards, remote write targets, and long-term storage | AL-2. Anyone who can query the metrics sees every tenant | Supports TLS and basic authentication on its own endpoint (AL-1) |
| EX-3 | **TLS to the databases** | Results include tenant names, IDs, and addresses | Passes `tls=` in the connection URL through openstackdb |
| EX-4 | **Credential storage and rotation** | The connection URLs hold the passwords | Reads them once at startup. Rotation needs a restart. Never logs them |
| EX-5 | **Network policy** that limits who can reach the listener port, and limits the exporter's egress to the databases | AL-1 | Exposes a single port (`Dockerfile:14`) |
| EX-6 | **Container runtime hardening**: a non-root user, a read-only root file system, no added capabilities | R-3. The image doesn't set a user | The image holds only the binary and the CA bundle |
| EX-7 | **Safe rendering of label values** in dashboards, alert templates, and notifications | Label values are tenant-chosen text | Exposition escaping only. No HTML or template escaping |

## Residual risks

Accepted, with what would retire each.

| ID | Risk | Bounded by | Retired by |
| --- | --- | --- | --- |
| R-1 | **No query deadline and no scrape limit.** Every `Collect` uses `context.Background()`, and `promhttp.HandlerOpts{}` (`main.go:109`) sets no `Timeout` or `MaxRequestsInFlight`. A slow database or a burst of scrapes piles up queries and goroutines, and an abandoned scrape keeps its queries running | Each pool caps open connections at 5 (10 for Keystone and Placement, which have two pools each), through openstackdb `Connect`. The optional rate limiter in `--web.config.file` | A per-scrape deadline on every query, and `Timeout` and `MaxRequestsInFlight` on the handler |
| R-2 | **No HTTP server timeouts.** The server is `&http.Server{}` (`main.go:127`), so slow clients can hold connections open | Network policy (EX-5) | `ReadHeaderTimeout`, `ReadTimeout`, and `IdleTimeout` on the server |
| R-3 | **The image runs as root.** The `Dockerfile` has no `USER` instruction | The image is `scratch`, with no shell or other tools. The runtime can set the user (EX-6) | A numeric non-root `USER` in the `Dockerfile` |
| R-4 | **Invalid UTF-8 crashes the exporter.** Label values pass unvalidated to `prometheus.MustNewConstMetric`, which panics on invalid UTF-8, and nothing recovers the panic | OpenStack APIs accept names as JSON strings, which are valid UTF-8. The process restarts under its supervisor | Validating or sanitizing label values, or using `prometheus.NewConstMetric` and dropping the series on error |
| R-5 | **Series count grows with tenant resources.** Per-resource series for servers, volumes, images, ports, and similar objects | Tenant quotas | Per-resource series behind a flag, or limits in the Prometheus scrape configuration |
| R-6 | **Table-level grants expose secret columns.** Tables the exporter reads also hold secrets, such as `instances.user_data`, `nodes.driver_info`, `nodes.instance_info`, `nodes.driver_internal_info`, `volumes.provider_auth`, `snapshots.provider_auth`, `volume_attachment.connection_info`, and `cluster.trustee_password` | The exporter never selects them. EX-1 | Column-level grants on those tables |
| R-7 | **Runner egress is only audited.** Every job runs `harden-runner` with `egress-policy: audit`. The `ci` workflow also grants `actions: read` and `security-events: write` (`.github/workflows/ci.yaml:10-13`), which none of its jobs use | Actions pinned by SHA. No job in `ci` handles registry credentials | `egress-policy: block` with an allowlist, and `contents: read` alone in `ci.yaml` |
| R-8 | **No Go vulnerability scan in CI.** Known vulnerabilities in Go modules show up only through Renovate updates | Renovate | A `govulncheck` job in `ci.yaml` |
| R-9 | **Integration tests pull `mariadb:11` by tag** (`internal/testutil/integration.go`) | Test-only. The CI jobs have no write access to packages | Pinning the test image by digest |
| R-10 | **The nightly scan may not cover the published image.** `periodic.yaml` scans the `edge` tag, which `image.yaml` doesn't publish | Renovate | Scan a tag that `image.yaml` publishes, or add `type=edge` to its tags |
| R-11 | **Images are built on a third-party remote builder.** `image.yaml` builds and pushes through Depot (`step-security/depot-setup-action` and `depot/build-push-action`), authenticated with the job's `id-token: write` | Actions pinned by SHA. Push only outside pull requests in the default-branch workflow | Building on GitHub-hosted runners, or verifying the builder's output |
| R-12 | **No image signature or provenance attestation.** `image.yaml` has no signing step and no explicit provenance or SBOM attestation | In this repository only `image.yaml` requests `packages: write`, and it pushes only outside pull requests. Anyone with write access to the repository can change workflows, so branch protection and review of workflow changes bound it | Signing images and publishing build provenance in `image.yaml` |
| R-13 | **Renovate behavior is defined outside this repository.** `renovate.json` only extends the shared `local>vexxhost/.github` preset, so schedules and any automerge come from that preset | Pull request review of Renovate changes, where the preset requires it | Pinning the relevant Renovate settings, such as automerge, in `renovate.json` |

## Fixed findings

Findings are listed here only once their fix has shipped. Open findings stay out of this public repository: the full report lives in `local/security-review/<date>/` (gitignored), and a finding in upstream code is reported privately (to VEXXHOST, or through a draft repository security advisory) before it's disclosed.

| ID | Severity | Finding | Fixed in |
| --- | --- | --- | --- |
| — | — | None yet | — |
