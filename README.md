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
