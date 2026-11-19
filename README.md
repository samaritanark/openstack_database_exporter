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

## Cinder quotas

Cinder quota collection uses SELECT-only `openstackdb` repository reads. Supply
Cinder and Keystone database URLs with read-only credentials for project names
and `domain_id` labels. Without Keystone metadata, `tenant` falls back to
the project ID and `domain_id` is empty. All DB-discovered projects, including
usage-only projects, and cached Keystone projects are included.

Set `--cinder.quota-query-timeout` / `CINDER_QUOTA_QUERY_TIMEOUT` (default `5s`)
shorter than the scrape timeout. All quota SQL reads share this deadline.
`openstack_cinder_quota_up=1` indicates successful collection, not necessarily
complete coverage of every individual quota scope. Isolated malformed records
are logged and omitted as described below. Database query failures, timeouts,
and unrecoverable global errors fail closed: quota_up is 0 with no quota data.
The existing `openstack_cinder_up` inventory health gauge remains separate.
The existing resolver's Keystone refresh is separate from this SQL deadline.

Add a complete `cinder` section to `--quota.defaults-file` / `QUOTA_DEFAULTS_FILE`:

```json
{
  "cinder": {
    "volumes": 10,
    "snapshots": 10,
    "backups": 10,
    "groups": 10,
    "gigabytes": 1000,
    "backup_gigabytes": 1000,
    "per_volume_gigabytes": -1,
    "use_default_quota_class": true
  }
}
```

These are upstream 2025.2 configuration defaults; mirror any customized Cinder
settings (`per_volume_gigabytes` corresponds to `per_volume_size_limit`). Existing
Octavia files remain valid: absent Cinder settings select upstream defaults
with a warning. `use_default_quota_class` is optional and defaults to true.
Project overrides take precedence over the active `default` quota-class entries,
then deployment defaults. Set the switch false to mirror Cinder's corresponding
option. Type resources default independently to -1, rather than inheriting the
aggregate project limit. Request-context custom quota classes and alternative
quota drivers cannot be inferred from the database; this collector models the
standard DbQuotaDriver with no custom request-context quota class.

All quota data has `tenant,tenant_id,domain_id`; type data also has `volume_type`
(the full type name). **Compatibility:** `domain_id` is added to existing Cinder
quota/capacity metrics, preserving `tenant` and `tenant_id`; inventory labels are
unchanged. The metric families below have the `openstack_cinder_` prefix:

| Families | Measurements |
| --- | --- |
| `quota_volumes`, `quota_snapshots`, `quota_backups`, `quota_groups` | `type=limit,in_use,reserved` |
| `quota_per_volume_bytes` | `type=limit` only |
| `limits_volume_max_bytes`, `limits_volume_used_bytes`, `limits_volume_reserved_bytes` | Aggregate byte limit, in-use and reserved |
| `limits_backup_max_bytes`, `limits_backup_used_bytes`, `limits_backup_reserved_bytes` | Backup byte limit, in-use and reserved |
| `volume_type_quota_volumes`, `volume_type_quota_snapshots` | `type=limit,in_use,reserved` |
| `volume_type_quota_bytes` | Type byte limit |
| `volume_type_quota_in_use_bytes`, `volume_type_quota_reserved_bytes` | Type byte usage and reservations |

No duplicate project capacity quota families are emitted. New type measurements
cover project/type scopes observed in active overrides or accounting; only the
existing type gigabyte limit preserves its legacy project × active-type series.
For P projects, T active types and S observed project/type scopes, the maximum
series count is `19P + P*T + 8S + 1`. Active type names are resolved without
splitting underscores; deleted types are excluded. No type ID label is added,
which preserves the existing type identity label contract.

Usage and reservations are the recorded enforcement accounting, not physical
backend consumption. Aggregate bytes normally include volumes **and**
snapshots; `no_snapshot_gb_quota` changes Cinder's accounting, which the exporter
reads as maintained. Type and aggregate measurements overlap: do not add them.
Reservations are not additionally summed from reservation records or expired by
the exporter. Independent SQL reads are not an atomic quota-check transaction.

Zero limits are preserved; -1 means unlimited. The project's NULL override falls
through to defaults, as in `DbQuotaDriver.get_project_quotas`; a NULL default-class
limit cannot supply a valid inherited limit. Invalid or duplicate default-class
records omit only dependent limits; explicit valid project overrides and recorded
accounting remain available. NULL or empty default resource names are ignored.
NULL/empty volume-type names and duplicate active names omit the affected types.
No ambiguous record is selected and no invalid limit is replaced by zero or a
configuration fallback. Duplicate project/resource limits or usage omit only the
affected scope; other valid metrics remain available. Negative `in_use` and
`reserved` counters remain signed and capacity values scale to bytes. Malformed project identities and
project limits below -1 still fail collection.
Active rows use Cinder's Boolean `deleted=0`.

For alerts, use `(in_use + reserved) / limit` only for positive finite limits,
and gate on quota health; zero and unlimited limits need separate handling.
Aggregate project usage by domain once, choosing a single exporter source where
API and DB exporters expose identical capacity names. Summed project limits are
allocated project quotas, not a shared domain enforcement limit.

Semantics were checked against release-tagged Cinder 27.0.0 (2025.2):
[quota engines/defaults](https://github.com/openstack/cinder/blob/27.0.0/cinder/quota.py),
[accounting and synchronization](https://github.com/openstack/cinder/blob/27.0.0/cinder/db/sqlalchemy/api.py),
and [models](https://github.com/openstack/cinder/blob/27.0.0/cinder/db/sqlalchemy/models.py).
Model comments calling NULL unlimited conflict with the actual driver path;
the behavior above follows that path. Quota values are never repaired.


### Cinder capacity migration

This change removes six existing capacity family names without aliases or dual
emission. Update dashboards, recording rules and alerts to the following names.
All numeric capacities use bytes: source GiB × 1073741824 (2^30). Conversion
uses floating point before multiplication. Only unlimited quota **limits** retain
`-1`; signed usage, reservations and inventory values scale normally.

| Previous family | Byte family |
| --- | --- |
| `openstack_cinder_limits_volume_max_gb` | `openstack_cinder_limits_volume_max_bytes` |
| `openstack_cinder_limits_volume_used_gb` | `openstack_cinder_limits_volume_used_bytes` |
| `openstack_cinder_limits_backup_max_gb` | `openstack_cinder_limits_backup_max_bytes` |
| `openstack_cinder_limits_backup_used_gb` | `openstack_cinder_limits_backup_used_bytes` |
| `openstack_cinder_volume_type_quota_gigabytes` | `openstack_cinder_volume_type_quota_bytes` |
| `openstack_cinder_volume_gb` | `openstack_cinder_volume_bytes` |

The first five families also gain `domain_id` alongside their existing identity
labels. This changes series identity; adjust aggregations and joins accordingly.
The new capacity families `openstack_cinder_limits_volume_reserved_bytes`,
`openstack_cinder_limits_backup_reserved_bytes`,
`openstack_cinder_quota_per_volume_bytes`,
`openstack_cinder_volume_type_quota_in_use_bytes` and
`openstack_cinder_volume_type_quota_reserved_bytes` are introduced directly in
bytes. Configuration and SQL resource keys still use `gigabytes`, and the
`openstack_cinder_volume_status` **size label stays in GiB**. Volume inventory
keeps its existing labels and attachment-row behavior.
