package manila

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
)

func TestQuotaCapacityBytes(t *testing.T) {
	for _, raw := range []int64{-1, 0, 1, 20, 2147483647} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close db: %v", err)
				}
			}()
			rows := emptyQuotaRows()
			for _, r := range manilaQuotaResources {
				if !strings.HasSuffix(r.name, "gigabytes") {
					continue
				}
				rows[1].AddRow("p", r.name, raw)
				// Exceeds the int64 multiplication range after conversion to bytes.
				rows[3].AddRow("p", r.name, int64(1<<40), -1)
				if r.shareType {
					rows[4].AddRow("p", "gold-id", "gold", r.name, raw)
					rows[5].AddRow("p", "gold-id", "gold", r.name, 20, -2)
				}
			}
			expectQuotaRows(mock, rows)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), nil))
			families, err := reg.Gather()
			require.NoError(t, err)
			capacities := 0
			for _, f := range families {
				name := f.GetName()
				require.NotContains(t, name, "gigabytes")
				if !strings.HasSuffix(name, "_bytes") {
					continue
				}
				capacities++
				require.Contains(t, f.GetHelp(), "source GiB")
				for _, m := range f.Metric {
					labels := map[string]string{}
					for _, l := range m.Label {
						labels[l.GetName()] = l.GetValue()
					}
					require.Equal(t, "p", labels["tenant_id"])
					require.Equal(t, "p", labels["tenant"])
					require.Contains(t, labels, "domain_id")
					want := map[string]float64{"limit": float64(raw) * (1 << 30), "in_use": 1 << 70, "reserved": -(1 << 30)}[labels["type"]]
					if raw == -1 && labels["type"] == "limit" {
						want = -1
					}
					if strings.Contains(name, "share_type") {
						require.Len(t, labels, 6)
						require.Equal(t, "gold-id", labels["share_type"])
						require.Equal(t, "gold", labels["share_type_name"])
						if labels["type"] == "in_use" {
							want = 20 * (1 << 30)
						}
						if labels["type"] == "reserved" {
							want = -2 * (1 << 30)
						}
					} else {
						require.Len(t, labels, 4)
					}
					require.Equal(t, want, m.GetGauge().GetValue(), name)
				}
			}
			require.Equal(t, 8, capacities)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
