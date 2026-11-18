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

## Cinder volumes

`openstack_cinder_volume_gb` and `openstack_cinder_volume_status` emit one
sample per volume. Capacity remains in GiB, and the status metric's `size` label
retains the raw GiB value. The `server_id` label is empty for no known server,
a single server UUID for one server, or a comma-separated, lexically sorted set
of distinct server UUIDs for multiple servers. This retains multiattachment
metadata without counting capacity more than once. Queries matching a single
`server_id` exactly will not match volumes with multiple servers.

The attachment query excludes soft-deleted attachments. It does not expose
attachment status or timestamps, so nondeleted historical records still
contribute their server IDs. Duplicate join rows do not affect samples, volume
counts, or status counts. Volume and status counts count distinct volume IDs.
