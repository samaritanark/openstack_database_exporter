package nova

import (
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	api "github.com/vexxhost/openstackdb/nova/db/api"
	queries "github.com/vexxhost/openstackdb/nova/db/main"
)

func TestInventoryCapacityBytes(t *testing.T) {
	for _, raw := range []int32{-1, 0, 1, 20, 2147483647} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close db: %v", err)
				}
			}()
			rows := sqlmock.NewRows([]string{"id", "uuid", "display_name", "user_id", "project_id", "host", "availability_zone", "vm_state", "power_state", "task_state", "memory_mb", "vcpus", "root_gb", "ephemeral_gb", "launched_at", "terminated_at", "instance_type_id", "deleted"}).AddRow(1, "id", "name", "u", "p", "host", "az", "active", 1, nil, 2048, 2, raw, 99, nil, nil, 1, 0)
			mock.ExpectQuery(regexp.QuoteMeta(queries.InstanceGetAll)).WillReturnRows(rows)
			mock.ExpectQuery(regexp.QuoteMeta(api.FlavorGetAll)).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(&serverCollectorWrapper{NewServerCollector(logger, queries.New(db), api.New(db))})
			families, err := reg.Gather()
			require.NoError(t, err)
			seen := map[string]bool{}
			for _, f := range families {
				name := f.GetName()
				seen[name] = true
				require.NotEqual(t, "openstack_nova_server_local_gb", name)
				if name == "openstack_nova_server_local_bytes" {
					require.Len(t, f.Metric, 1)
					require.Equal(t, "Server root disk capacity in bytes (source root_gb in GiB multiplied by 2^30).", f.GetHelp())
					require.Equal(t, dto.MetricType_GAUGE, f.GetType())
					m := f.Metric[0]
					require.NotNil(t, m.Gauge)
					require.Equal(t, float64(raw)*(1<<30), m.GetGauge().GetValue())
					labels := map[string]string{}
					for _, l := range m.Label {
						labels[l.GetName()] = l.GetValue()
					}
					require.Equal(t, map[string]string{"id": "id", "name": "name", "tenant_id": "p"}, labels)
				}
				if name == "openstack_nova_server_status" {
					require.Len(t, f.Metric, 1)
				}
				if name == "openstack_nova_total_vms" {
					require.EqualValues(t, 1, f.Metric[0].GetGauge().GetValue())
				}
			}
			require.True(t, seen["openstack_nova_server_local_bytes"])
			require.True(t, seen["openstack_nova_server_status"])
			require.True(t, seen["openstack_nova_total_vms"])
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
