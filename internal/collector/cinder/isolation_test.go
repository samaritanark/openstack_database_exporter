package cinder

import (
	"bytes"
	"log/slog"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func requireNoQuotaSample(t *testing.T, families map[string]*dto.MetricFamily, name string, labels map[string]string) {
	t.Helper()
	for _, metric := range families["openstack_cinder_"+name].GetMetric() {
		actual := map[string]string{}
		for _, label := range metric.Label {
			actual[label.GetName()] = label.GetValue()
		}
		matches := true
		for key, value := range labels {
			if actual[key] != value {
				matches = false
			}
		}
		require.False(t, matches, "%s emitted skipped labels %v", name, actual)
	}
}

func TestQuotasMalformedVolumeTypesIsolated(t *testing.T) {
	for _, bad := range []string{"null", "empty", "duplicate"} {
		t.Run(bad, func(t *testing.T) {
			db, mock, _, resolver := newQuotaTest(t)
			rows := quotaRows()
			switch bad {
			case "null":
				rows[3].AddRow("invalid-type", nil)
			case "empty":
				rows[3].AddRow("invalid-type", "")
			case "duplicate":
				rows[3].AddRow("a", "bad").AddRow("b", "bad").AddRow("c", "bad")
			}
			rows[3].AddRow("good-type", "fast_ssd")
			rows[0].AddRow("p", "volumes_bad", 99).AddRow("p", "gigabytes_bad", 999)
			rows[1].AddRow("p", "volumes_bad", 7, 2).AddRow("p", "gigabytes_bad", 77, 22)
			rows[0].AddRow("p", "volumes_fast_ssd", 4).AddRow("p", "gigabytes_fast_ssd", 40)
			rows[1].AddRow("p", "volumes_fast_ssd", 1, 2).AddRow("p", "gigabytes_fast_ssd", 3, 4)
			rows[0].AddRow("healthy", "volumes", 20).AddRow("healthy", "gigabytes", 500)
			rows[1].AddRow("healthy", "gigabytes", 120, 3)
			expectQuotaRows(mock, rows, true)
			var logs bytes.Buffer
			f := gatherQuotas(t, NewQuotasCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), resolver))
			require.EqualValues(t, 1, quotaSample(t, f, "quota_up", nil))
			require.Contains(t, logs.String(), "operation=\"validate volume types\"")
			if bad == "duplicate" {
				require.Contains(t, logs.String(), "volume_type=bad")
			} else {
				require.Contains(t, logs.String(), "volume_type_id=invalid-type")
			}
			require.EqualValues(t, 20, quotaSample(t, f, "quota_volumes", map[string]string{"tenant_id": "healthy", "type": "limit"}))
			require.EqualValues(t, 500*(1<<30), quotaSample(t, f, "limits_volume_max_bytes", map[string]string{"tenant_id": "healthy"}))
			require.EqualValues(t, 120*(1<<30), quotaSample(t, f, "limits_volume_used_bytes", map[string]string{"tenant_id": "healthy"}))
			require.EqualValues(t, 40*(1<<30), quotaSample(t, f, "volume_type_quota_bytes", map[string]string{"tenant_id": "p", "volume_type": "fast_ssd"}))
			require.EqualValues(t, 4, quotaSample(t, f, "volume_type_quota_volumes", map[string]string{"tenant_id": "p", "volume_type": "fast_ssd", "type": "limit"}))
			for _, name := range []string{"volume_type_quota_volumes", "volume_type_quota_snapshots", "volume_type_quota_bytes", "volume_type_quota_in_use_bytes", "volume_type_quota_reserved_bytes"} {
				requireNoQuotaSample(t, f, name, map[string]string{"volume_type": "bad"})
			}
		})
	}
}

func TestQuotasMalformedDefaultsIsolated(t *testing.T) {
	for _, resource := range []string{"volumes", "gigabytes", "volumes_fast_ssd", "gigabytes_fast_ssd"} {
		for _, malformed := range []string{"null", "negative", "duplicate", "equal duplicates", "invalid then valid"} {
			t.Run(resource+"/"+malformed, func(t *testing.T) {
				db, mock, _, resolver := newQuotaTest(t)
				rows := quotaRows()
				switch malformed {
				case "null":
					rows[2].AddRow(resource, nil)
				case "negative":
					rows[2].AddRow(resource, -2)
				case "duplicate":
					rows[2].AddRow(resource, 5).AddRow(resource, 6).AddRow(resource, 7)
				case "equal duplicates":
					rows[2].AddRow(resource, 5).AddRow(resource, 5)
				case "invalid then valid":
					rows[2].AddRow(resource, nil).AddRow(resource, 6)
				}
				rows[2].AddRow("snapshots", 31)
				rows[3].AddRow("vt", "fast_ssd")
				rows[0].AddRow("override", resource, 0).AddRow("unlimited", resource, -1).AddRow("inherits", resource, nil)
				rows[1].AddRow("inherits", resource, 2, 3).AddRow("inherits", "backup_gigabytes", 10, 4)
				expectQuotaRows(mock, rows, true)
				var logs bytes.Buffer
				f := gatherQuotas(t, NewQuotasCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), resolver))
				require.EqualValues(t, 1, quotaSample(t, f, "quota_up", nil))
				require.Contains(t, logs.String(), "resource="+resource)
				require.Contains(t, logs.String(), "project_id=inherits")
				require.Contains(t, logs.String(), "invalid shared default")
				require.EqualValues(t, 31, quotaSample(t, f, "quota_snapshots", map[string]string{"tenant_id": "inherits", "type": "limit"}))
				require.EqualValues(t, 1000*(1<<30), quotaSample(t, f, "limits_backup_max_bytes", map[string]string{"tenant_id": "inherits"}))
				require.EqualValues(t, 10*(1<<30), quotaSample(t, f, "limits_backup_used_bytes", map[string]string{"tenant_id": "inherits"}))
				name, used, reserved := "quota_volumes", "quota_volumes", "quota_volumes"
				labels := map[string]string{"tenant_id": "inherits", "type": "limit"}
				if resource == "volumes_fast_ssd" || resource == "gigabytes_fast_ssd" {
					labels["volume_type"] = "fast_ssd"
					name, used, reserved = "volume_type_quota_volumes", "volume_type_quota_volumes", "volume_type_quota_volumes"
				}
				switch resource {
				case "gigabytes":
					name, used, reserved = "limits_volume_max_bytes", "limits_volume_used_bytes", "limits_volume_reserved_bytes"
					delete(labels, "type")
				case "gigabytes_fast_ssd":
					name, used, reserved = "volume_type_quota_bytes", "volume_type_quota_in_use_bytes", "volume_type_quota_reserved_bytes"
					delete(labels, "type")
				}
				requireNoQuotaSample(t, f, name, labels)
				for pid, limit := range map[string]int{"override": 0, "unlimited": -1} {
					labels["tenant_id"] = pid
					require.EqualValues(t, limit, quotaSample(t, f, name, labels))
				}
				labels["tenant_id"] = "inherits"
				if name == used {
					labels["type"] = "in_use"
				}
				factor := 1
				if resource == "gigabytes" || resource == "gigabytes_fast_ssd" {
					factor = 1 << 30
				}
				require.EqualValues(t, 2*factor, quotaSample(t, f, used, labels))
				if name == reserved {
					labels["type"] = "reserved"
				}
				require.EqualValues(t, 3*factor, quotaSample(t, f, reserved, labels))
			})
		}
	}
}

func TestQuotasMalformedDefaultIdentityIgnored(t *testing.T) {
	db, mock, _, resolver := newQuotaTest(t)
	rows := quotaRows()
	rows[2].AddRow(nil, 90).AddRow("", 90).AddRow("unknown", nil)
	rows[1].AddRow("p", "volumes", 2, 3).AddRow("p", "gigabytes", 30, 4)
	expectQuotaRows(mock, rows, true)
	var logs bytes.Buffer
	f := gatherQuotas(t, NewQuotasCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), resolver))
	require.EqualValues(t, 1, quotaSample(t, f, "quota_up", nil))
	require.EqualValues(t, 10, quotaSample(t, f, "quota_volumes", map[string]string{"type": "limit"}))
	require.EqualValues(t, 30*(1<<30), quotaSample(t, f, "limits_volume_used_bytes", nil))
	require.Contains(t, logs.String(), "null or empty resource")
}
