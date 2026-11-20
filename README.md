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

### Neutron project quotas


The existing `openstack_neutron_quota_floatingip`, `quota_router`,
`quota_network`, `quota_port`, `quota_security_group`, and
`quota_security_group_rule` families report `type="limit|used|reserved"` (each
suffix has the `openstack_neutron_` prefix). All quota families now have
`tenant,tenant_id,domain_id,type` labels. The original Neutron `type="used"`
convention is preserved for all nine families, without aliases. Additional
identity labels change series identity: review exact label matching, joins,
aggregations and recording rules, even where existing `type="used"` selectors
still work. Group by `tenant_id` for project identity and `domain_id` for domain
attribution. Names are not unique. Unknown Keystone owners retain their project
ID as `tenant` and an empty `domain_id`; no domain is inferred from a name.

This implementation assumes **Neutron 2025.2 DbQuotaNoLockDriver**. Usage counts
rows owned directly by each project; reservations sum active joined
`reservations`/`resourcedeltas` rows with `expiration >=` one UTC database-clock cutoff per scrape.
Expired and NULL-expiration reservations are excluded. Multiple expiration
groups contribute to the same total, matching corrected 27.0.4 aggregation.
It does not use `quotausages` or support legacy/null/custom driver accounting.
Separate SQL reads can race with mutations.

Extend the shared `--quota.defaults-file` JSON with:

```json
{"neutron":{"floatingip":50,"router":10,"network":100,"port":500,"security_group":10,"security_group_rule":100}}
```

A supplied `neutron` section requires these six integer values and optionally
accepts `subnet`, `subnetpool`, and `rbac_policy`. When omitted, these optional
defaults are `100`, `-1` (the upstream global `default_quota`), and `10`,
respectively. Existing six-key files remain valid. NULLs, missing required keys,
duplicate keys, trailing content, unknown resources and non-integers are
invalid. Zero is valid; negative configured defaults normalize to unlimited `-1`
as in Neutron. If no section/file is supplied, the collector logs its upstream
2025.2 default assumption (values shown above). Project overrides take
precedence, including zero and recorded negative limits. These defaults are
independent of other services. SQL alone cannot discover deployment
configuration.

`--neutron.quota-query-timeout` (`NEUTRON_QUOTA_QUERY_TIMEOUT`, default `5s`)
provides a positive shared deadline for quota SQL reads. Optional metadata
refresh is bounded by the remaining time in that deadline. Set it below the
scrape timeout. `openstack_neutron_quota_up` is 1 only after required SQL reads
succeed; query, timeout or global configuration failures emit only quota health
0. Isolated malformed records omit affected scopes with health 1. It remains
registered after a connection failure, separately from inventory
`openstack_neutron_up`. Missing metadata uses cached data or project-ID
fallback; refresh failures are logged without database error text. Metadata
freshness is not certified by quota health. A metadata refresh that exhausts the
remaining deadline does not discard valid quota samples or set quota health to
0. Invalid defaults or timeout settings are logged once at startup with
structured source, known key and reason fields; raw configuration, file paths,
unknown key text and driver errors are not logged. Invalid Neutron-section
defaults or timeout settings leave unrelated metrics operational. Shared-file
parsing failures may still prevent startup when another service rejects the
file.

Supported-resource NULL overrides, duplicate project/resource quota or usage records, duplicate reservation
deltas, invalid reservation amounts and negative usage/reservation values omit all three
measurements for the affected `(project_id, resource)`. Neutron applies stored
NULL overrides directly; they do not imply inheritance. Zero and negative
(unlimited) limits remain valid. Missing owners conservatively omit the affected
resource across projects; missing resources omit that owner's resources. If
neither identifier is usable, all quota scopes are omitted. Later records cannot
restore invalidated scopes. Unknown extension resources are ignored for accounting
but their owners are discovered. Successful empty SQL results establish legitimate
zero usage/reservations. All nine resources, including `subnet`, `subnetpool` and
`rbac_policy`, report actual active reservations (27 series per healthy project).

`openstack_neutron_quota_skipped_records` is an unlabeled gauge emitted on
successful SQL collection, including zero on clean scrapes. It counts returned
records that fail validation, including each extra duplicate row; an invalid
usage aggregate counts once; each malformed raw reservation delta counts once. The first
otherwise valid row in an ambiguous group is not counted again. It does not count
omitted metric samples, SQL-filtered records, metadata failures or unsupported
extensions. It is absent when collection fails globally. Warnings use controlled
reasons, allowlisted resource names and hashed owner identifiers, with at most one
warning per reason per scrape.

Neutron stores naive UTC expiration timestamps. The reservation query uses
`UTC_TIMESTAMP()` once per statement, independently of the exporter clock and
connection location. Explicitly empty usage and reservation owners are
system-owned and are skipped. NULL and whitespace owners returned by SQL
remain invalid; SQL-filtered records are not counted as skipped.

Metadata fallback can temporarily change series identity until Keystone enrichment
succeeds. Validate dashboard queries and alerts against the new identity labels
before production rollout.

Before rollout, confirm the deployed driver, effective defaults, enabled resource
plugins, reservation schema, Keystone access, query plans and API patch version.
Do not sum unlimited limits as finite capacity. Use one authoritative quota
exporter target to avoid double counting overlapping API/database metrics.
