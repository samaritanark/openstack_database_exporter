package nova

import (
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	keystone "github.com/vexxhost/openstackdb/keystone/db"
	novaapi "github.com/vexxhost/openstackdb/nova/db/api"
	placement "github.com/vexxhost/openstackdb/placement/objects"
)

func TestQuotaAndLimitsCapacityBytes(t *testing.T) {
	for _, limits := range []bool{false, true} {
		for _, raw := range []int64{-1, 0, 1, 20, 2147483647} {
			t.Run(fmt.Sprintf("limits=%v/raw=%d", limits, raw), func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer func() {
					mock.ExpectClose()
					if err := db.Close(); err != nil {
						t.Errorf("close db: %v", err)
					}
				}()
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				mock.ExpectQuery(regexp.QuoteMeta(keystone.ListProjects)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).AddRow("p", "project", "", true, "d", "", false, ""))
				resolver := project.NewResolver(logger, keystone.New(db), time.Hour)
				mock.ExpectQuery(regexp.QuoteMeta(novaapi.QuotaGetAll)).WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "resource", "hard_limit"}).AddRow(1, "p", "ram", raw))
				usage := raw
				if raw == 2147483647 {
					usage = 1 << 44
				}
				allocations := sqlmock.NewRows([]string{"project_id", "resource_type", "used"}).AddRow("p", "MEMORY_MB", usage).AddRow("p", "VCPU", 2)
				consumers := sqlmock.NewRows([]string{"project_id", "instance_count"}).AddRow("p", 3)
				defaults := sqlmock.NewRows([]string{"resource", "hard_limit"})
				if limits {
					mock.ExpectQuery(regexp.QuoteMeta(novaapi.QuotaClassGetDefaults)).WillReturnRows(defaults)
				}
				mock.ExpectQuery(regexp.QuoteMeta(placement.GetAllocationsByProject)).WillReturnRows(allocations)
				mock.ExpectQuery(regexp.QuoteMeta(placement.GetConsumerCountByProject)).WillReturnRows(consumers)
				if !limits {
					mock.ExpectQuery(regexp.QuoteMeta(novaapi.QuotaClassGetDefaults)).WillReturnRows(defaults)
				}
				reg := prometheus.NewPedanticRegistry()
				if limits {
					reg.MustRegister(&limitsCollectorWrapper{NewLimitsCollector(logger, nil, novaapi.New(db), placement.New(db), resolver)})
				} else {
					reg.MustRegister(&quotasCollectorWrapper{NewQuotasCollector(logger, nil, novaapi.New(db), placement.New(db), resolver)})
				}
				families, err := reg.Gather()
				require.NoError(t, err)
				wantLimit := float64(raw) * (1 << 20)
				if raw == -1 {
					wantLimit = -1
				}
				seen := map[string]bool{}
				for _, f := range families {
					name := f.GetName()
					seen[name] = true
					require.NotContains(t, []string{"openstack_nova_quota_ram", "openstack_nova_limits_memory_max", "openstack_nova_limits_memory_used"}, name)
					for _, m := range f.Metric {
						labels := map[string]string{}
						for _, l := range m.Label {
							labels[l.GetName()] = l.GetValue()
						}
						require.Equal(t, "d", labels["domain_id"])
						require.Equal(t, "project", labels["tenant"])
						require.Len(t, labels, 3)
						switch name {
						case "openstack_nova_quota_ram_bytes":
							require.Equal(t, "RAM quota in bytes (source MiB multiplied by 2^20); -1 means unlimited only for limits.", f.GetHelp())
							require.Equal(t, dto.MetricType_GAUGE, f.GetType())
							require.NotNil(t, m.Gauge)
							require.Contains(t, []string{"limit", "in_use", "reserved"}, labels["type"])
							require.Equal(t, map[string]string{"domain_id": "d", "tenant": "project", "type": labels["type"]}, labels)
							require.Equal(t, map[string]float64{"limit": wantLimit, "in_use": float64(usage) * (1 << 20), "reserved": 0}[labels["type"]], m.GetGauge().GetValue())
						case "openstack_nova_limits_memory_max_bytes":
							require.Equal(t, "Memory max in bytes (source MiB multiplied by 2^20); -1 means unlimited only for limits.", f.GetHelp())
							require.Equal(t, dto.MetricType_GAUGE, f.GetType())
							require.NotNil(t, m.Gauge)
							require.Equal(t, wantLimit, m.GetGauge().GetValue())
							require.Equal(t, "p", labels["tenant_id"])
						case "openstack_nova_limits_memory_used_bytes":
							require.Equal(t, "Memory used in bytes (source MiB multiplied by 2^20); -1 means unlimited only for limits.", f.GetHelp())
							require.Equal(t, dto.MetricType_GAUGE, f.GetType())
							require.NotNil(t, m.Gauge)
							require.EqualValues(t, float64(usage)*(1<<20), m.GetGauge().GetValue())
							require.Equal(t, "p", labels["tenant_id"])
						case "openstack_nova_quota_cores":
							require.Equal(t, map[string]float64{"limit": 20, "in_use": 2, "reserved": 0}[labels["type"]], m.GetGauge().GetValue())
						case "openstack_nova_quota_injected_file_content_bytes":
							require.Equal(t, map[string]float64{"limit": 10240, "in_use": 0, "reserved": 0}[labels["type"]], m.GetGauge().GetValue())
						case "openstack_nova_limits_vcpus_used":
							require.EqualValues(t, 2, m.GetGauge().GetValue())
						case "openstack_nova_limits_instances_used":
							require.EqualValues(t, 3, m.GetGauge().GetValue())
						}
					}
				}
				if limits {
					require.True(t, seen["openstack_nova_limits_memory_max_bytes"])
					require.True(t, seen["openstack_nova_limits_memory_used_bytes"])
				} else {
					require.True(t, seen["openstack_nova_quota_ram_bytes"])
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
