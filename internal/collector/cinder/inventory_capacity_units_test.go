package cinder

import (
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	queries "github.com/vexxhost/openstackdb/cinder/db"
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
			rows := sqlmock.NewRows([]string{"id", "name", "size", "status", "availability_zone", "bootable", "project_id", "user_id", "volume_type", "server_id"}).AddRow("id", "name", raw, "available", "az", true, "p", "u", "gold", nil)
			mock.ExpectQuery(regexp.QuoteMeta(queries.VolumeGetAllWithAttachments)).WillReturnRows(rows)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(NewVolumesCollector(db, logger))
			families, err := reg.Gather()
			require.NoError(t, err)
			seen := map[string]bool{}
			for _, f := range families {
				name := f.GetName()
				seen[name] = true
				require.NotEqual(t, "openstack_cinder_volume_gb", name)
				if name == "openstack_cinder_volume_bytes" {
					require.Len(t, f.Metric, 1)
					require.Contains(t, f.GetHelp(), "GiB")
					m := f.Metric[0]
					require.NotNil(t, m.Gauge)
					require.Equal(t, float64(raw)*(1<<30), m.GetGauge().GetValue())
					labels := map[string]string{}
					for _, l := range m.Label {
						labels[l.GetName()] = l.GetValue()
					}
					require.Equal(t, map[string]string{"id": "id", "name": "name", "status": "available", "availability_zone": "az", "bootable": "true", "tenant_id": "p", "user_id": "u", "volume_type": "gold", "server_id": ""}, labels)
				}
				if name == "openstack_cinder_volume_status" {
					require.Len(t, f.Metric, 1)
					for _, l := range f.Metric[0].Label {
						if l.GetName() == "size" {
							require.Equal(t, strconv.FormatInt(int64(raw), 10), l.GetValue())
						}
					}
				}
				if name == "openstack_cinder_volumes" {
					require.EqualValues(t, 1, f.Metric[0].GetGauge().GetValue())
				}
			}
			require.True(t, seen["openstack_cinder_volume_bytes"])
			require.True(t, seen["openstack_cinder_volume_status"])
			require.True(t, seen["openstack_cinder_volumes"])
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
