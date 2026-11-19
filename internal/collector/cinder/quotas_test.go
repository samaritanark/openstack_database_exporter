package cinder

import (
	"bytes"
	"database/sql"
	"errors"
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
	cinderdb "github.com/vexxhost/openstackdb/cinder/db"
	keystonedb "github.com/vexxhost/openstackdb/keystone/db"
)

var quotaQueries = []string{cinderdb.QuotaGetActiveLimits, cinderdb.QuotaGetActiveUsage, cinderdb.QuotaGetDefaultClassLimits, cinderdb.VolumeTypeGetAllCurrent}

func quotaRows() []*sqlmock.Rows {
	return []*sqlmock.Rows{
		sqlmock.NewRows([]string{"project_id", "resource", "hard_limit"}),
		sqlmock.NewRows([]string{"project_id", "resource", "in_use", "reserved"}),
		sqlmock.NewRows([]string{"resource", "hard_limit"}),
		sqlmock.NewRows([]string{"id", "name"}),
	}
}
func expectQuotaRows(mock sqlmock.Sqlmock, rows []*sqlmock.Rows, class bool) {
	for i, q := range quotaQueries {
		if i == 2 && !class {
			continue
		}
		mock.ExpectQuery(regexp.QuoteMeta(q)).WillReturnRows(rows[i])
	}
}
func gatherQuotas(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	require.NoError(t, err)
	result := map[string]*dto.MetricFamily{}
	for _, f := range families {
		result[f.GetName()] = f
	}
	return result
}
func quotaSample(t *testing.T, f map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	family := f["openstack_cinder_"+name]
	require.NotNil(t, family, name)
	var found []*dto.Metric
	for _, m := range family.Metric {
		matches := true
		for k, v := range labels {
			present := false
			for _, l := range m.Label {
				if l.GetName() == k && l.GetValue() == v {
					present = true
				}
			}
			if !present {
				matches = false
			}
		}
		if matches {
			found = append(found, m)
		}
	}
	require.Len(t, found, 1, "%s %v", name, labels)
	return found[0].GetGauge().GetValue()
}
func newQuotaTest(t *testing.T) (*sql.DB, sqlmock.Sqlmock, *slog.Logger, *project.Resolver) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return db, mock, logger, project.NewResolver(logger, nil, 0)
}

func TestQuotasAccountingAndPrecedence(t *testing.T) {
	db, mock, logger, _ := newQuotaTest(t)
	// Resolver reads project metadata once; a missing project must retain ID labels.
	kdb, kmock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() {
		kmock.ExpectClose()
		if err := kdb.Close(); err != nil {
			t.Errorf("close kdb: %v", err)
		}
	}()
	kmock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).AddRow("p", "project", "", true, "partner", "", false, "").AddRow("idle", "idle-project", "", true, "partner", "", false, ""))
	resolver := project.NewResolver(logger, keystonedb.New(kdb), time.Hour)
	rows := quotaRows()
	rows[0].AddRow("p", "volumes", 0).AddRow("p", "snapshots", -1).AddRow("p", "gigabytes", 200).AddRow("p", "gigabytes_fast_ssd", 30).AddRow("typed-only", "volumes_fast_ssd", 8)
	for i, r := range []string{"volumes", "snapshots", "backups", "groups", "gigabytes", "backup_gigabytes"} {
		rows[1].AddRow("p", r, i+1, i+2)
	}
	rows[1].AddRow("usage-only", "backups", 7, 4).AddRow("p", "volumes_fast_ssd", 2, 3).AddRow("p", "snapshots_fast_ssd", 4, 1).AddRow("p", "gigabytes_fast_ssd", 9, 5).AddRow("p", "per_volume_gigabytes", 0, 0)
	rows[2].AddRow("volumes", 40).AddRow("gigabytes", 400).AddRow("snapshots_fast_ssd", 13)
	rows[3].AddRow("vt1", "fast_ssd").AddRow("vt2", "__DEFAULT__")
	expectQuotaRows(mock, rows, true)
	defaults := upstreamQuotaDefaults()
	defaults.Limits["backups"] = 77
	defaults.Limits["groups"] = 17
	collector := NewQuotasCollector(db, logger, resolver, QuotaOptions{Defaults: defaults})
	defaults.Limits["backups"] = 999 // Constructor owns a copy.
	f := gatherQuotas(t, collector)
	require.Len(t, f, 17)
	// Assert every quota family, gauge type, complete label set and state contract.
	for _, contract := range []struct {
		name          string
		typed, states bool
	}{
		{"quota_volumes", false, true}, {"quota_snapshots", false, true},
		{"quota_backups", false, true}, {"quota_groups", false, true},
		{"quota_per_volume_bytes", false, true},
		{"limits_volume_max_bytes", false, false}, {"limits_volume_used_bytes", false, false},
		{"limits_volume_reserved_bytes", false, false}, {"limits_backup_max_bytes", false, false},
		{"limits_backup_used_bytes", false, false}, {"limits_backup_reserved_bytes", false, false},
		{"volume_type_quota_volumes", true, true}, {"volume_type_quota_snapshots", true, true},
		{"volume_type_quota_bytes", true, false}, {"volume_type_quota_in_use_bytes", true, false},
		{"volume_type_quota_reserved_bytes", true, false},
	} {
		family := f["openstack_cinder_"+contract.name]
		require.NotNil(t, family, contract.name)
		require.Equal(t, dto.MetricType_GAUGE, family.GetType())
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			names := []string{"tenant", "tenant_id", "domain_id"}
			if contract.typed {
				names = append(names, "volume_type")
			}
			if contract.states {
				names = append(names, "type")
				if contract.name == "quota_per_volume_bytes" {
					require.Equal(t, "limit", labels["type"])
				} else {
					require.Contains(t, []string{"limit", "in_use", "reserved"}, labels["type"])
				}
			}
			require.Len(t, labels, len(names))
			for _, name := range names {
				require.Contains(t, labels, name)
			}
		}
	}
	require.Empty(t, f["openstack_cinder_quota_up"].Metric[0].Label)
	require.EqualValues(t, 1, quotaSample(t, f, "quota_up", nil))
	p := map[string]string{"tenant_id": "p", "tenant": "project", "domain_id": "partner", "type": "limit"}
	require.EqualValues(t, 0, quotaSample(t, f, "quota_volumes", p))
	require.EqualValues(t, -1, quotaSample(t, f, "quota_snapshots", p))
	require.EqualValues(t, 77, quotaSample(t, f, "quota_backups", p))
	require.EqualValues(t, 17, quotaSample(t, f, "quota_groups", p))
	require.EqualValues(t, -1, quotaSample(t, f, "quota_per_volume_bytes", p))
	require.Len(t, f["openstack_cinder_quota_per_volume_bytes"].Metric, 4) // project limit only.
	for i, r := range []string{"volumes", "snapshots", "backups", "groups"} {
		require.EqualValues(t, i+1, quotaSample(t, f, "quota_"+r, map[string]string{"tenant_id": "p", "type": "in_use"}))
		require.EqualValues(t, i+2, quotaSample(t, f, "quota_"+r, map[string]string{"tenant_id": "p", "type": "reserved"}))
	}
	require.EqualValues(t, 7, quotaSample(t, f, "quota_backups", map[string]string{"tenant_id": "usage-only", "tenant": "usage-only", "domain_id": "", "type": "in_use"}))
	require.EqualValues(t, 4, quotaSample(t, f, "quota_backups", map[string]string{"tenant_id": "usage-only", "type": "reserved"}))
	require.EqualValues(t, 40, quotaSample(t, f, "quota_volumes", map[string]string{"tenant_id": "idle", "type": "limit"}))
	require.EqualValues(t, 200*(1<<30), quotaSample(t, f, "limits_volume_max_bytes", map[string]string{"tenant_id": "p"}))
	require.EqualValues(t, 5*(1<<30), quotaSample(t, f, "limits_volume_used_bytes", map[string]string{"tenant_id": "p"}))
	require.EqualValues(t, 6*(1<<30), quotaSample(t, f, "limits_volume_reserved_bytes", map[string]string{"tenant_id": "p"}))
	require.EqualValues(t, 6*(1<<30), quotaSample(t, f, "limits_backup_used_bytes", map[string]string{"tenant_id": "p"}))
	require.EqualValues(t, 7*(1<<30), quotaSample(t, f, "limits_backup_reserved_bytes", map[string]string{"tenant_id": "p"}))
	require.EqualValues(t, 30*(1<<30), quotaSample(t, f, "volume_type_quota_bytes", map[string]string{"tenant_id": "p", "volume_type": "fast_ssd"}))
	require.EqualValues(t, 9*(1<<30), quotaSample(t, f, "volume_type_quota_in_use_bytes", map[string]string{"tenant_id": "p", "volume_type": "fast_ssd"}))
	require.EqualValues(t, 5*(1<<30), quotaSample(t, f, "volume_type_quota_reserved_bytes", map[string]string{"tenant_id": "p", "volume_type": "fast_ssd"}))
	require.EqualValues(t, -1, quotaSample(t, f, "volume_type_quota_volumes", map[string]string{"tenant_id": "p", "volume_type": "fast_ssd", "type": "limit"}))
	require.EqualValues(t, 13, quotaSample(t, f, "volume_type_quota_snapshots", map[string]string{"tenant_id": "p", "volume_type": "fast_ssd", "type": "limit"}))
	require.EqualValues(t, 8, quotaSample(t, f, "volume_type_quota_volumes", map[string]string{"tenant_id": "typed-only", "type": "limit"}))
	require.Len(t, f["openstack_cinder_volume_type_quota_volumes"].Metric, 6) // two observed scopes, not P*T.
	require.Len(t, f["openstack_cinder_volume_type_quota_bytes"].Metric, 8)   // legacy limits P*T retained.
	require.NoError(t, kmock.ExpectationsWereMet())
}

func TestQuotasDisableClassAndIgnoreInactiveTypes(t *testing.T) {
	db, mock, logger, resolver := newQuotaTest(t)
	rows := quotaRows()
	rows[1].AddRow("p", "volumes", 2, 1).AddRow("p", "volumes_deleted_type", 99, 2)
	rows[3].AddRow("active", "fast_ssd")
	expectQuotaRows(mock, rows, false)
	defaults := upstreamQuotaDefaults()
	defaults.UseDefaultQuotaClass = false
	defaults.Limits["volumes"] = 24
	f := gatherQuotas(t, NewQuotasCollector(db, logger, resolver, QuotaOptions{Defaults: defaults}))
	require.EqualValues(t, 24, quotaSample(t, f, "quota_volumes", map[string]string{"type": "limit"}))
	require.NotContains(t, f, "openstack_cinder_volume_type_quota_volumes")
}

func TestQuotasRejectAmbiguousAccounting(t *testing.T) {
	tests := map[string]func([]*sqlmock.Rows){
		"invalid negative limit": func(r []*sqlmock.Rows) { r[0].AddRow("p", "volumes", -2) },
		"empty project":          func(r []*sqlmock.Rows) { r[1].AddRow("", "volumes", 1, 1) },
		"null project":           func(r []*sqlmock.Rows) { r[0].AddRow(nil, "volumes", 1) },
		"null resource":          func(r []*sqlmock.Rows) { r[1].AddRow("p", nil, 1, 1) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			db, mock, logger, resolver := newQuotaTest(t)
			rows := quotaRows()
			mutate(rows)
			expectQuotaRows(mock, rows, true)
			f := gatherQuotas(t, NewQuotasCollector(db, logger, resolver))
			require.Len(t, f, 1)
			require.Zero(t, quotaSample(t, f, "quota_up", nil))
		})
	}
}

func TestQuotasFailuresAndTimeouts(t *testing.T) {
	for failAt := range quotaQueries {
		t.Run(quotaQueries[failAt][:20], func(t *testing.T) {
			db, mock, _, resolver := newQuotaTest(t)
			rows := quotaRows()
			for i := 0; i < failAt; i++ {
				mock.ExpectQuery(regexp.QuoteMeta(quotaQueries[i])).WillReturnRows(rows[i])
			}
			mock.ExpectQuery(regexp.QuoteMeta(quotaQueries[failAt])).WillReturnError(errors.New("mysql://secret:password@host"))
			var logs bytes.Buffer
			f := gatherQuotas(t, NewQuotasCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), resolver))
			require.Len(t, f, 1)
			require.Zero(t, quotaSample(t, f, "quota_up", nil))
			require.NotContains(t, logs.String(), "password")
		})
	}
	t.Run("deadline covers usage read", func(t *testing.T) {
		db, mock, logger, resolver := newQuotaTest(t)
		rows := quotaRows()
		mock.ExpectQuery(regexp.QuoteMeta(quotaQueries[0])).WillReturnRows(rows[0])
		mock.ExpectQuery(regexp.QuoteMeta(quotaQueries[1])).WillDelayFor(time.Second).WillReturnRows(rows[1])
		start := time.Now()
		f := gatherQuotas(t, NewQuotasCollector(db, logger, resolver, QuotaOptions{QueryTimeout: 200 * time.Millisecond}))
		require.Less(t, time.Since(start), time.Second)
		require.Len(t, f, 1)
		require.Zero(t, quotaSample(t, f, "quota_up", nil))
	})
	t.Run("invalid defaults", func(t *testing.T) {
		db, _, logger, resolver := newQuotaTest(t)
		f := gatherQuotas(t, NewQuotasCollector(db, logger, resolver, QuotaOptions{Defaults: &QuotaDefaults{}}))
		require.Len(t, f, 1)
		require.Zero(t, quotaSample(t, f, "quota_up", nil))
	})
}

func TestQuotasNullProjectLimitInherits(t *testing.T) {
	db, mock, logger, resolver := newQuotaTest(t)
	rows := quotaRows()
	rows[0].AddRow("p", "volumes", nil).AddRow("p", "gigabytes", nil).AddRow("p", "volumes_fast_ssd", nil)
	rows[2].AddRow("volumes", 23).AddRow("gigabytes", 321).AddRow("volumes_fast_ssd", 4)
	rows[3].AddRow("t", "fast_ssd")
	expectQuotaRows(mock, rows, true)
	f := gatherQuotas(t, NewQuotasCollector(db, logger, resolver))
	require.EqualValues(t, 23, quotaSample(t, f, "quota_volumes", map[string]string{"type": "limit"}))
	require.EqualValues(t, 321*(1<<30), quotaSample(t, f, "limits_volume_max_bytes", nil))
	require.EqualValues(t, 4, quotaSample(t, f, "volume_type_quota_volumes", map[string]string{"type": "limit"}))
}

func TestMandatoryQuotasUsageOnlyProjects(t *testing.T) {
	db, mock, logger, resolver := newQuotaTest(t)
	rows := quotaRows()
	for _, r := range []string{"volumes", "snapshots", "backups"} {
		rows[1].AddRow("only-usage", r, 6, 3)
	}
	expectQuotaRows(mock, rows, true)
	f := gatherQuotas(t, NewQuotasCollector(db, logger, resolver))
	for _, r := range []string{"volumes", "snapshots", "backups"} {
		for state, want := range map[string]float64{"limit": 10, "in_use": 6, "reserved": 3} {
			require.Equal(t, want, quotaSample(t, f, "quota_"+r, map[string]string{"tenant_id": "only-usage", "domain_id": "", "type": state}))
		}
	}
}

func TestQuotasNegativeCountersPassThrough(t *testing.T) {
	for _, counters := range []struct {
		name            string
		inUse, reserved int
	}{
		{"negative usage", -1, 0},
		{"negative reservation", 1, -1},
	} {
		t.Run(counters.name, func(t *testing.T) {
			db, mock, logger, resolver := newQuotaTest(t)
			rows := quotaRows()
			for _, resource := range []string{"volumes", "gigabytes", "gigabytes_fast_ssd"} {
				rows[1].AddRow("desynced", resource, counters.inUse, counters.reserved)
			}
			rows[0].AddRow("healthy", "volumes", 20).AddRow("healthy", "gigabytes", 500)
			rows[1].AddRow("healthy", "gigabytes", 120, 3)
			rows[3].AddRow("vt", "fast_ssd")
			expectQuotaRows(mock, rows, true)
			f := gatherQuotas(t, NewQuotasCollector(db, logger, resolver))
			require.EqualValues(t, 1, quotaSample(t, f, "quota_up", nil))
			for state, want := range map[string]int{"in_use": counters.inUse, "reserved": counters.reserved} {
				require.EqualValues(t, want, quotaSample(t, f, "quota_volumes", map[string]string{"tenant_id": "desynced", "type": state}))
			}
			for name, want := range map[string]int{
				"limits_volume_used_bytes":         counters.inUse,
				"limits_volume_reserved_bytes":     counters.reserved,
				"volume_type_quota_in_use_bytes":   counters.inUse,
				"volume_type_quota_reserved_bytes": counters.reserved,
			} {
				require.EqualValues(t, int64(want)*(1<<30), quotaSample(t, f, name, map[string]string{"tenant_id": "desynced"}))
			}
			require.EqualValues(t, 20, quotaSample(t, f, "quota_volumes", map[string]string{"tenant_id": "healthy", "type": "limit"}))
			require.EqualValues(t, 500*(1<<30), quotaSample(t, f, "limits_volume_max_bytes", map[string]string{"tenant_id": "healthy"}))
			require.EqualValues(t, 120*(1<<30), quotaSample(t, f, "limits_volume_used_bytes", map[string]string{"tenant_id": "healthy"}))
		})
	}
}

func TestQuotasDuplicateRowsIsolateResource(t *testing.T) {
	for _, resource := range []string{"volumes", "gigabytes", "gigabytes_fast_ssd"} {
		for _, source := range []string{"limits", "null limits", "usage"} {
			t.Run(resource+"/"+source, func(t *testing.T) {
				db, mock, _, resolver := newQuotaTest(t)
				rows := quotaRows()
				if source == "usage" {
					rows[0].AddRow("affected", resource, 99)
					rows[1].AddRow("affected", resource, 1, 2).AddRow("affected", resource, 3, 4).AddRow("affected", resource, 5, 6)
				} else {
					var first any = 1
					if source == "null limits" {
						first = nil
					}
					rows[0].AddRow("affected", resource, first).AddRow("affected", resource, 2).AddRow("affected", resource, 3)
					rows[1].AddRow("affected", resource, 5, 6)
				}
				// Other resources of this project and the same resource in another
				// type/project remain available, including the legacy capacity series.
				rows[0].AddRow("affected", "snapshots", 21).AddRow("affected", "gigabytes_gold", 70)
				rows[0].AddRow("healthy", "volumes", 20).AddRow("healthy", "gigabytes", 500).AddRow("healthy", "gigabytes_fast_ssd", 50)
				rows[1].AddRow("healthy", "gigabytes", 120, 3).AddRow("affected", "backup_gigabytes", 10, 2)
				rows[3].AddRow("vt", "fast_ssd").AddRow("vt2", "gold")
				expectQuotaRows(mock, rows, true)
				var logs bytes.Buffer
				f := gatherQuotas(t, NewQuotasCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), resolver))
				require.EqualValues(t, 1, quotaSample(t, f, "quota_up", nil))
				require.Contains(t, logs.String(), "project_id=affected")
				require.Contains(t, logs.String(), "resource="+resource)
				require.NotContains(t, logs.String(), "hard_limit")
				require.EqualValues(t, 21, quotaSample(t, f, "quota_snapshots", map[string]string{"tenant_id": "affected", "type": "limit"}))
				require.EqualValues(t, 10*(1<<30), quotaSample(t, f, "limits_backup_used_bytes", map[string]string{"tenant_id": "affected"}))
				require.EqualValues(t, 70*(1<<30), quotaSample(t, f, "volume_type_quota_bytes", map[string]string{"tenant_id": "affected", "volume_type": "gold"}))
				require.EqualValues(t, 20, quotaSample(t, f, "quota_volumes", map[string]string{"tenant_id": "healthy", "type": "limit"}))
				require.EqualValues(t, 500*(1<<30), quotaSample(t, f, "limits_volume_max_bytes", map[string]string{"tenant_id": "healthy"}))
				require.EqualValues(t, 120*(1<<30), quotaSample(t, f, "limits_volume_used_bytes", map[string]string{"tenant_id": "healthy"}))
				require.EqualValues(t, 3*(1<<30), quotaSample(t, f, "limits_volume_reserved_bytes", map[string]string{"tenant_id": "healthy"}))
				require.EqualValues(t, 50*(1<<30), quotaSample(t, f, "volume_type_quota_bytes", map[string]string{"tenant_id": "healthy", "volume_type": "fast_ssd"}))
				names := map[string][]string{
					"volumes":            {"quota_volumes"},
					"gigabytes":          {"limits_volume_max_bytes", "limits_volume_used_bytes", "limits_volume_reserved_bytes"},
					"gigabytes_fast_ssd": {"volume_type_quota_bytes", "volume_type_quota_in_use_bytes", "volume_type_quota_reserved_bytes"},
				}
				for _, name := range names[resource] {
					for _, metric := range f["openstack_cinder_"+name].Metric {
						labels := map[string]string{}
						for _, label := range metric.Label {
							labels[label.GetName()] = label.GetValue()
						}
						if labels["tenant_id"] == "affected" {
							if resource == "gigabytes_fast_ssd" {
								require.NotEqual(t, "fast_ssd", labels["volume_type"], name)
							} else {
								t.Errorf("ambiguous resource emitted: %s %v", name, labels)
							}
						}
					}
				}
			})
		}
	}
}
