# OpenStack Database Exporter

This is a Prometheus exporter that aims to pull some of the data that is
provided by the [OpenStack Exporter](https://github.com/openstack-exporter/openstack-exporter)
but directly from the database.

## Why?

The existance of this exporter is an OpenStack bug.  At scale, the APIs
become extremely sluggish at operating with large number of API requests.

In addition, certain queries can be done far more effectively directly
without going through many different API calls.  The goal of this exporter
is to be operator-facing and providing high performance for large scale
clouds.

## Database queries

Service database APIs, SQL definitions, and integration schema fixtures are
maintained in [openstackdb](https://github.com/vexxhost/openstackdb). This exporter
owns Prometheus collectors and metric-output tests. Query changes belong in
openstackdb; update the pinned module version here after validating compatibility.

```sh
go test -short -race ./...
# Requires Docker for MariaDB fixtures and exporter end-to-end tests.
go test -race -tags integration -timeout 15m ./...
go build ./cmd/openstack-database-exporter
```

Integration tests load embedded fixtures from the pinned openstackdb module.
Database URLs and exporter configuration remain the same.

## Octavia project quotas

The Octavia collector exports `openstack_loadbalancer_quota_{loadbalancer,listener,pool,member,healthmonitor,l7policy,l7rule}`
as gauges with `tenant`, `tenant_id`, `domain_id` and `type` labels. Every
resource has `type="limit"` and `type="in_use"`; no reserved values are emitted.
Usage counts objects in each resource table with a nonempty project ID,
excluding `provisioning_status='DELETED'`. ERROR and transitional states count.
These are project counts, not per-load-balancer counts or traffic statistics.

Object counts can differ from Octavia's cached `quotas.in_use_*` values, which
Octavia normally uses for quota enforcement. Drift can persist until corrected;
these metrics do not guarantee that Octavia will accept another creation.
Before enabling quota alerts, compare representative per-project counts with
the service API, the cached accounting values and effective service defaults
on a representative database snapshot. Investigate persistent differences rather
than assuming they will resolve. Gate usage alerts on
`openstack_loadbalancer_quota_up == 1`. For utilization ratios, filter limits
with `limit > 0`: `-1` represents unlimited and `0` prohibits allocation.
Zero-limit violations need a separate `in_use > 0` check.

Configure `--octavia.database-url` / `OCTAVIA_DATABASE_URL` and, for project names,
domains and idle projects, `--keystone.database-url` / `KEYSTONE_DATABASE_URL`.
Without available project metadata, observed project IDs are retained as `tenant`
and `tenant_id`, and `domain_id` is empty. Idle projects cannot be discovered from
Octavia alone. The project cache retains last successful metadata on a refresh
failure; cached names/domains can therefore be stale. Orphaned projects in Octavia
remain visible. Every known project has 14 quota samples; the success gauge has
no labels.

Octavia configuration defaults are not stored in its database. The optional
`--quota.defaults-file` / `QUOTA_DEFAULTS_FILE` supplies all seven effective
`[quotas]` defaults. With no path, the exporter starts and logs a warning that
it assumes Octavia 2025.2 upstream unlimited (`-1`) defaults for every resource.
If service defaults are customized, generate this file from the same values
that render `octavia.conf` and update both together to avoid configuration drift.
Only the Octavia section is read; unrelated service sections are ignored:

```json
{
  "octavia": {
    "loadbalancer": -1,
    "listener": -1,
    "pool": -1,
    "member": -1,
    "healthmonitor": -1,
    "l7policy": -1,
    "l7rule": -1
  }
}
```

Map `loadbalancer` and `healthmonitor` to service settings
`default_load_balancer_quota` and `default_health_monitor_quota`; the other five
map to `default_<resource>_quota`. An explicit project value always wins over
the configured default; a NULL or missing override uses that default. Zero
and `-1` are preserved. An explicitly supplied unreadable, malformed or
incomplete file fails startup with a configuration error when Octavia is enabled.
When Octavia is disabled, the file is ignored. Unknown Octavia resource names,
NULLs, fractions, overflow and values below `-1` are rejected.
Programmatic `collector.Config` callers may omit `OctaviaQuotaDefaults` (nil)
to use upstream defaults with a warning, or supply a complete map.

`openstack_loadbalancer_quota_up` is an unlabeled gauge: `1` when quota
collection succeeds (including an empty database), `0` when it fails. Both
quota queries must succeed and resource identifiers must be recognized before
any quota limit or usage sample is emitted. Failures log the operation and
safe error classification, omit all quota samples and preserve unrelated
metrics and a successful HTTP scrape. A failed initial Octavia connection also
reports quota success as `0`; when Octavia is disabled this gauge is absent.
Existing `openstack_loadbalancer_up` inventory health remains separate from quota health.
The two quota database reads share a 5-second context deadline, configurable via
`--octavia.quota-query-timeout` / `OCTAVIA_QUOTA_QUERY_TIMEOUT` (a positive Go
duration, for example `4s`). Set this below your scrape timeout with room for
other collection work and response delivery. The default assumes a scrape budget
longer than five seconds; the exporter does not discover Prometheus settings.
Other collectors and the existing Keystone metadata refresh retain their own
behavior, so this is not a global scrape deadline. No schema indexes or
migrations are added.

Measure query plans and latency on a representative snapshot before rollout.

These metrics require the public [openstackdb Octavia quota APIs](https://github.com/samaritanark/openstackdb/pull/2).
That contribution is awaiting acceptance upstream; the tracked v0.2.1 dependency
does not contain these APIs. Builds and tests currently require a temporary Go
workspace selecting that public library source alongside this exporter.

## Manila quotas and logical share inventory

The Manila collector exports project quota gauges with `tenant`, `tenant_id`,
`domain_id` and `type` labels. The twelve families are:

- `openstack_sharev2_quota_shares`
- `openstack_sharev2_quota_bytes`
- `openstack_sharev2_quota_snapshots`
- `openstack_sharev2_quota_snapshot_bytes`
- `openstack_sharev2_quota_share_networks`
- `openstack_sharev2_quota_share_replicas`
- `openstack_sharev2_quota_replica_bytes`
- `openstack_sharev2_quota_share_groups`
- `openstack_sharev2_quota_share_group_snapshots`
- `openstack_sharev2_quota_backups`
- `openstack_sharev2_quota_backup_bytes`
- `openstack_sharev2_quota_per_share_bytes`

Six corresponding `openstack_sharev2_share_type_quota_` families use suffixes
`shares`, `bytes`, `snapshots`, `snapshot_bytes`, `share_replicas` and
`replica_bytes`. They add `share_type` (UUID) and `share_type_name` labels.
All quota families expose `type="limit"`, `type="in_use"` and `type="reserved"`,
except `quota_per_share_bytes`, which exposes only `limit`. There is no user
label. Only the limit value `-1` means unlimited; zero prohibits allocation.

Usage comes from Manila's persisted quota accounting, including signed usage
and reservations, rather than reconstructing usage from shares. Project totals
sum per-user rows with no share-type scope; share-type totals are kept separate
because they describe the same allocations. Reservations already in accounting
are not added again. These values describe provisioned quota consumption, not
physical backend consumption. The reads share a deadline but are not a single
transactional snapshot.

Limits use configured defaults, then the persisted `default` quota class, then
project overrides, then share-type overrides. Share-type limits inherit project
limits where no type override exists. User overrides discover project owners
but never replace project limits. Usage-only and override-only owners remain
visible. Configure Keystone for project names, domains and idle projects;
without metadata the owner ID becomes `tenant` and `domain_id` is empty.
Share-type scopes are discovered from limits or usage for active share types.

Use the existing `--quota.defaults-file` / `QUOTA_DEFAULTS_FILE` for effective
Manila `[quota]` settings. Source capacity settings remain in GiB:

```json
{
  "manila": {
    "shares": 50,
    "gigabytes": 1000,
    "snapshots": 50,
    "snapshot_gigabytes": 1000,
    "share_networks": 10,
    "share_replicas": 100,
    "replica_gigabytes": 1000,
    "share_groups": 50,
    "share_group_snapshots": 50,
    "backups": 10,
    "backup_gigabytes": 1000,
    "per_share_gigabytes": -1
  }
}
```

A supplied Manila section must contain all twelve integer values, each at least
`-1`; unknown fields, NULLs, fractions and overflow fail startup. No file, or a
file containing only another service section, uses the Manila 2025.2 defaults
above and logs a warning. Keep customized defaults synchronized with
`manila.conf`. Disabled Manila ignores its defaults and timeout validation.
`--manila.quota-query-timeout` / `MANILA_QUOTA_QUERY_TIMEOUT` sets the shared
deadline for the six quota reads (default `5s`, positive Go duration). Leave
room within the scrape timeout for inventory and project metadata collection.

`openstack_sharev2_quota_up` is an unlabeled gauge: `1` for complete quota
collection, including an empty database, and `0` on failure. Query failures or
invalid shared defaults omit quota data. Invalid project or share-type limits
omit the affected scope (and types inheriting an invalid project) while retaining
valid scopes with `quota_up=0`. Gate quota alerts on this gauge. Inventory health
remains `openstack_sharev2_up`; a quota failure can leave inventory and other
services available through HTTP 200. An initial connection failure reports both
health gauges as zero.

Inventory uses `ShareGetInventory` to emit one record per logical share,
including zero-instance shares and retained soft-deleted shares whose row is
still nondeleted. Instance status priority and active-replica preference select
one representative instance, with the replication-change/reverting exception
and instance ID tie-breaking. Status, type and availability zone come from that
same instance; missing instances leave that metadata empty. Status and count
metric names and label keys remain unchanged. The `size` label remains in GiB.

The existing inventory family `openstack_sharev2_share_gb` is renamed to
`openstack_sharev2_share_bytes`, preserving its labels. Update dashboards and
recording rules to the new name. All Manila capacity values now multiply source
GiB by `2^30`, including negative usage, reservations and inventory values;
only an unlimited quota **limit** of `-1` stays unscaled. No legacy alias is
exported.

This contribution also requires compatible public
[openstackdb Manila APIs](https://github.com/samaritanark/openstackdb/pull/3),
including `QuotaRepository` with scoped usage totals and `ShareGetInventory`,
alongside the inherited Octavia APIs. The tracked v0.2.1 module lacks them.
A temporary external Go workspace is needed for pre-merge validation; verify
API compatibility of the selected library revisions before building. Library
acceptance and dependency reconciliation remain prerequisites for ordinary CI.
