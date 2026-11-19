package cinder

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLimitsCollectorCompatibility(t *testing.T) {
	db, mock, logger, resolver := newQuotaTest(t)
	rows := quotaRows()
	rows[0].AddRow("p", "gigabytes_gold", 300)
	rows[1].AddRow("p", "gigabytes", 250, 12).AddRow("p", "backup_gigabytes", 50, 3)
	rows[3].AddRow("vt", "gold")
	expectQuotaRows(mock, rows, true)
	f := gatherQuotas(t, NewLimitsCollector(db, logger, resolver))
	for name, value := range map[string]float64{
		"limits_volume_max_bytes": 1000, "limits_volume_used_bytes": 250, "limits_volume_reserved_bytes": 12,
		"limits_backup_max_bytes": 1000, "limits_backup_used_bytes": 50, "limits_backup_reserved_bytes": 3,
		"volume_type_quota_bytes": 300, "volume_type_quota_in_use_bytes": 0, "volume_type_quota_reserved_bytes": 0,
	} {
		require.Equal(t, value*(1<<30), quotaSample(t, f, name, map[string]string{"tenant": "p", "tenant_id": "p", "domain_id": ""}))
	}
	require.NotContains(t, f, "openstack_cinder_quota_volumes")
}
