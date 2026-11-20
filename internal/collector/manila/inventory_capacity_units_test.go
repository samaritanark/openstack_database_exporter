package manila

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
	queries "github.com/vexxhost/openstackdb/manila/db"
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
			t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()) })
			rows := sqlmock.NewRows([]string{"id", "name", "project_id", "size", "share_proto", "status", "share_type", "share_type_name", "availability_zone"}).AddRow("id", "name", "p", raw, "NFS", "available", "gold-id", "gold", "az")
			mock.ExpectQuery(regexp.QuoteMeta(queries.ShareGetInventory)).WillReturnRows(rows)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(NewSharesCollector(db, logger))
			families, err := reg.Gather()
			require.NoError(t, err)
			seen := map[string]bool{}
			for _, f := range families {
				name := f.GetName()
				seen[name] = true
				require.NotEqual(t, "openstack_sharev2_share_gb", name)
				if name == "openstack_sharev2_up" {
					require.Len(t, f.Metric, 1)
					require.EqualValues(t, 1, f.Metric[0].GetGauge().GetValue())
				}
				if name == "openstack_sharev2_share_bytes" {
					require.Len(t, f.Metric, 1)
					require.Contains(t, f.GetHelp(), "GiB")
					m := f.Metric[0]
					require.NotNil(t, m.Gauge)
					require.Equal(t, float64(raw)*(1<<30), m.GetGauge().GetValue())
					labels := map[string]string{}
					for _, l := range m.Label {
						labels[l.GetName()] = l.GetValue()
					}
					require.Equal(t, map[string]string{"id": "id", "name": "name", "status": "available", "availability_zone": "az", "project_id": "p", "share_type": "gold-id", "share_type_name": "gold", "share_proto": "NFS"}, labels)
				}
				if name == "openstack_sharev2_share_status" {
					require.Len(t, f.Metric, 1)
					for _, l := range f.Metric[0].Label {
						if l.GetName() == "size" {
							require.Equal(t, strconv.FormatInt(int64(raw), 10), l.GetValue())
						}
					}
				}
				if name == "openstack_sharev2_shares_counter" {
					require.EqualValues(t, 1, f.Metric[0].GetGauge().GetValue())
				}
			}
			require.True(t, seen["openstack_sharev2_share_bytes"])
			require.True(t, seen["openstack_sharev2_share_status"])
			require.True(t, seen["openstack_sharev2_shares_counter"])
			require.True(t, seen["openstack_sharev2_up"])
		})
	}
}
