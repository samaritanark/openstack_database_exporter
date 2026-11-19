package cinder

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuotaCapacityBytes(t *testing.T) {
	for _, raw := range []int32{-1, 0, 1, 20, 2147483647} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			db, mock, logger, resolver := newQuotaTest(t)
			rows := quotaRows()
			for _, resource := range []string{"gigabytes", "backup_gigabytes", "per_volume_gigabytes", "gigabytes_gold"} {
				rows[0].AddRow("p", resource, raw)
				rows[1].AddRow("p", resource, raw, -1)
			}
			rows[3].AddRow("vt", "gold")
			expectQuotaRows(mock, rows, true)
			families := gatherQuotas(t, NewQuotasCollector(db, logger, resolver))
			capacities := 0
			for name, f := range families {
				require.NotContains(t, name, "gigabytes")
				require.False(t, strings.HasSuffix(name, "_gb"))
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
					count := 3
					if strings.Contains(name, "volume_type") {
						count++
						require.Equal(t, "gold", labels["volume_type"])
					}
					if strings.Contains(name, "per_volume") {
						count++
						require.Equal(t, "limit", labels["type"])
					}
					require.Len(t, labels, count)
					want := float64(raw) * (1 << 30)
					limit := strings.Contains(name, "_max_") || name == "openstack_cinder_volume_type_quota_bytes" || labels["type"] == "limit"
					if limit && raw == -1 {
						want = -1
					}
					if strings.Contains(name, "reserved") {
						want = -(1 << 30)
					}
					require.Equal(t, want, m.GetGauge().GetValue(), name)
				}
			}
			require.Equal(t, 10, capacities)
			require.EqualValues(t, 1, quotaSample(t, families, "quota_up", nil))
			require.EqualValues(t, 10, quotaSample(t, families, "quota_volumes", map[string]string{"type": "limit"}))
		})
	}
}
