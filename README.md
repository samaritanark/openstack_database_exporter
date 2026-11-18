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

## Breaking Nova capacity metric migration

Nova RAM, memory limits and server root-disk capacity gauges now use bytes.
Old names are removed, with no parallel legacy families. Labels, quota
calculations, defaults and scrape failure behavior are unchanged. The `ram`
resource key remains in its original OpenStack units; only emitted capacities
change.

| Old metric name | New metric name | Source → bytes |
| --- | --- | --- |
| `openstack_nova_quota_ram` | `openstack_nova_quota_ram_bytes` | MiB × 2^20 |
| `openstack_nova_limits_memory_max` | `openstack_nova_limits_memory_max_bytes` | MiB × 2^20 |
| `openstack_nova_limits_memory_used` | `openstack_nova_limits_memory_used_bytes` | MiB × 2^20 |
| `openstack_nova_server_local_gb` | `openstack_nova_server_local_bytes` | GiB × 2^30 |

Nova RAM and memory capacities are MiB × 1048576 (2^20). Only quota and memory
limits preserve `-1` as unlimited. Zero remains zero; signed usage, including
`-1`, is scaled. Server root-disk capacity uses GiB × 1073741824 (2^30), with
no unlimited sentinel. Conversion uses floating-point multiplication after
reading the source integer, avoiding integer overflow; Prometheus float64
precision still applies.

Quota RAM retains `domain_id`, `tenant`, `type` labels and
`limit`/`in_use`/`reserved` states. Memory limits retain `domain_id`, `tenant`,
`tenant_id`; server root-disk capacity retains `id`, `name`, `tenant_id`.
Usage still comes from Placement and quota reservations remain zero.
`server_local_bytes` still measures only `instances.root_gb`; it does not add
ephemeral disks, swap, or actual disk consumption. Server status, task-state,
CPU and count metrics are unchanged.

Update dashboard queries, alert thresholds, recording rules, metric allowlists,
and remote consumers together. Remove any old GiB/MiB-to-byte multipliers;
scale thresholds expressed in source units by the matching factor. Filter
unlimited limits before arithmetic (for example,
`openstack_nova_quota_ram_bytes{type="limit"} >= 0`). Historical series retain
their old names and units and need explicit handling in queries spanning the
rollout.
