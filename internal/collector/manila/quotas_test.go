package manila

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	keystonedb "github.com/vexxhost/openstackdb/keystone/db"
	maniladb "github.com/vexxhost/openstackdb/manila/db"
)

var quotaQueries = []struct {
	query   string
	columns []string
}{
	{maniladb.QuotaClassGetDefault, []string{"resource", "hard_limit"}},
	{maniladb.QuotaGetAllByProject, []string{"project_id", "resource", "hard_limit"}},
	{maniladb.QuotaGetAllByProjectAndUser, []string{"project_id", "user_id", "resource", "hard_limit"}},
	{maniladb.QuotaUsageGetAllByProject, []string{"project_id", "user_id", "resource", "in_use", "reserved"}},
	{maniladb.QuotaGetAllByProjectAndShareType, []string{"project_id", "share_type_id", "share_type_name", "resource", "hard_limit"}},
	{maniladb.QuotaUsageGetAllByProjectAndShareType, []string{"project_id", "share_type_id", "share_type_name", "user_id", "resource", "in_use", "reserved"}},
}

func emptyQuotaRows() []*sqlmock.Rows {
	rows := make([]*sqlmock.Rows, len(quotaQueries))
	for i, q := range quotaQueries {
		rows[i] = sqlmock.NewRows(q.columns)
	}
	return rows
}

func expectQuotaRows(mock sqlmock.Sqlmock, rows []*sqlmock.Rows) {
	for i, q := range quotaQueries {
		mock.ExpectQuery(regexp.QuoteMeta(q.query)).WillReturnRows(rows[i])
	}
}

func TestQuotasCollector(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	metaDB, metaMock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = metaDB.Close() }()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metaMock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).
			AddRow("p1", "partner", "", 1, "d1", "", 0, "").AddRow("idle", "idle-partner", "", 1, "d2", "", 0, ""))
	resolver := project.NewResolver(logger, keystonedb.New(metaDB), time.Hour)
	rows := emptyQuotaRows()
	rows[0].AddRow("shares", 777).AddRow("snapshot_gigabytes", 555).AddRow("backups", -1).AddRow("encryption_keys", 100)
	rows[1].AddRow("p1", "shares", 0).AddRow("p1", "gigabytes", -1).AddRow("p1", "snapshots", -1).AddRow("p1", "per_share_gigabytes", 64).
		AddRow("", "shares", 900).AddRow(nil, "shares", 900)
	rows[2].AddRow("p1", "u1", "shares", 1).AddRow("user-only", "u2", "shares", 3)
	for i, resource := range manilaQuotaResources {
		if !resource.limitOnly {
			rows[3].AddRow("p1", "u1", resource.name, 2000000000, 1).
				AddRow("p1", "u2", resource.name, 2000000000+i, i+2)
		}
	}
	rows[3].AddRow("usage-only", "u1", "shares", 4, 2).AddRow("p1", nil, "encryption_keys", 100, 0)
	rows[4].AddRow("p1", "t1", "gold", "shares", 5).AddRow("p1", "t1", "gold", "gigabytes", -1).
		AddRow("p1", "t1", "gold", "snapshots", 0).AddRow("type-limit-only", "t1", "gold", "shares", -1)
	for i, resource := range manilaQuotaResources {
		if resource.shareType {
			rows[5].AddRow("p1", "t1", "gold", nil, resource.name, i+10, 2)
		}
	}
	rows[5].AddRow("p1", "t2", "silver", nil, "shares", 6, 1).AddRow("type-usage-only", "t2", "silver", nil, "shares", 3, 1)
	expectQuotaRows(mock, rows)
	defaults := upstreamQuotaDefaults()
	defaults["share_groups"] = 23
	collector := NewQuotasCollector(db, logger, resolver, defaults)
	defaults["share_groups"] = 999 // Caller mutation cannot affect a scrape.
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 19)
	projectIDs := []string{"p1", "idle", "user-only", "usage-only", "type-limit-only", "type-usage-only"}
	samples := make(map[string]float64)
	for _, family := range families {
		name := family.GetName()
		if name == "openstack_sharev2_quota_up" {
			require.EqualValues(t, 1, family.Metric[0].GetGauge().GetValue())
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			require.Contains(t, projectIDs, labels["tenant_id"])
			require.Contains(t, labels, "domain_id")
			switch labels["tenant_id"] {
			case "p1":
				require.Equal(t, "partner", labels["tenant"])
				require.Equal(t, "d1", labels["domain_id"])
			case "idle":
				require.Equal(t, "idle-partner", labels["tenant"])
				require.Equal(t, "d2", labels["domain_id"])
			default:
				require.Equal(t, labels["tenant_id"], labels["tenant"])
				require.Empty(t, labels["domain_id"])
			}
			if strings.HasPrefix(name, "openstack_sharev2_share_type_quota_") {
				require.Len(t, labels, 6)
				require.Equal(t, map[string]string{"t1": "gold", "t2": "silver"}[labels["share_type"]], labels["share_type_name"])
			} else {
				require.Len(t, labels, 4)
			}
			key := strings.Join([]string{name, labels["tenant_id"], labels["share_type"], labels["type"]}, "/")
			require.NotContains(t, samples, key)
			samples[key] = metric.GetGauge().GetValue()
		}
	}
	effective := upstreamQuotaDefaults()
	effective["shares"], effective["snapshot_gigabytes"], effective["backups"], effective["share_groups"] = 777, 555, -1, 23
	projectLimits := map[string]int32{"shares": 0, "gigabytes": -1, "snapshots": -1, "per_share_gigabytes": 64}
	expectedCount := 0
	check := func(name, pid, tid, kind string, value float64) {
		t.Helper()
		if strings.HasSuffix(name, "gigabytes") {
			name = strings.TrimSuffix(name, "gigabytes") + "bytes"
			if kind != "limit" || value != -1 {
				value *= 1 << 30
			}
		}
		key := strings.Join([]string{name, pid, tid, kind}, "/")
		require.Contains(t, samples, key)
		require.Equal(t, value, samples[key], key)
		expectedCount++
	}
	for _, pid := range projectIDs {
		for i, r := range manilaQuotaResources {
			limit := effective[r.name]
			if v, ok := projectLimits[r.name]; pid == "p1" && ok {
				limit = v
			}
			name := "openstack_sharev2_quota_" + r.name
			check(name, pid, "", "limit", float64(limit))
			if !r.limitOnly {
				used, reserved := float64(0), float64(0)
				if pid == "p1" {
					used, reserved = 4000000000+float64(i), float64(i+3)
				}
				if pid == "usage-only" && r.name == "shares" {
					used, reserved = 4, 2
				}
				check(name, pid, "", "in_use", used)
				check(name, pid, "", "reserved", reserved)
			}
		}
	}
	for _, pair := range [][2]string{{"p1", "t1"}, {"p1", "t2"}, {"type-limit-only", "t1"}, {"type-usage-only", "t2"}} {
		for i, r := range manilaQuotaResources {
			if !r.shareType {
				continue
			}
			pid, tid := pair[0], pair[1]
			limit := effective[r.name]
			if v, ok := projectLimits[r.name]; pid == "p1" && ok {
				limit = v
			}
			if pid == "p1" && tid == "t1" {
				if v, ok := map[string]int32{"shares": 5, "gigabytes": -1, "snapshots": 0}[r.name]; ok {
					limit = v
				}
			}
			if pid == "type-limit-only" && r.name == "shares" {
				limit = -1
			}
			used, reserved := float64(0), float64(0)
			if pid == "p1" && tid == "t1" {
				used, reserved = float64(i+10), 2
			}
			if pid == "p1" && tid == "t2" && r.name == "shares" {
				used, reserved = 6, 1
			}
			if pid == "type-usage-only" && r.name == "shares" {
				used, reserved = 3, 1
			}
			name := "openstack_sharev2_share_type_quota_" + r.name
			check(name, pid, tid, "limit", float64(limit))
			check(name, pid, tid, "in_use", used)
			check(name, pid, tid, "reserved", reserved)
		}
	}
	require.Len(t, samples, expectedCount) // No per-share usage or extra scopes.
	require.NoError(t, mock.ExpectationsWereMet())
	require.NoError(t, metaMock.ExpectationsWereMet())
}

func TestQuotasCollectorFailures(t *testing.T) {
	for index := range quotaQueries {
		for _, scenario := range []string{"query error", "scan error", "iteration error", "timeout"} {
			t.Run(quotaQueries[index].columns[0]+"/"+strings.Fields(quotaQueries[index].query)[2]+"/"+scenario, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer func() { _ = db.Close() }()
				rows := emptyQuotaRows()
				rows[1].AddRow("p", "shares", 1)
				for i := 0; i < index; i++ {
					mock.ExpectQuery(regexp.QuoteMeta(quotaQueries[i].query)).WillReturnRows(rows[i])
				}
				query := mock.ExpectQuery(regexp.QuoteMeta(quotaQueries[index].query))
				switch scenario {
				case "query error":
					query.WillReturnError(errors.New("mysql://private-user:secret@private-host/db"))
				case "scan error":
					query.WillReturnRows(sqlmock.NewRows([]string{"wrong_columns"}).AddRow("bad"))
				case "iteration error":
					rows := sqlmock.NewRows([]string{"wrong_columns"}).AddRow("bad").RowError(0, sql.ErrConnDone)
					query.WillReturnRows(rows)
				case "timeout":
					query.WillDelayFor(3 * time.Second).WillReturnRows(rows[index])
				}
				var logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, nil))
				reg := prometheus.NewPedanticRegistry()
				timeout := 5 * time.Second
				if scenario == "timeout" {
					timeout = time.Second
				}
				reg.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), nil, timeout))
				// Retained Manila inventory succeeds independently of quota failure.
				mock.ExpectQuery(regexp.QuoteMeta(maniladb.ShareGetInventory)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "project_id", "size", "share_proto", "status", "share_type", "share_type_name", "availability_zone"}))
				reg.MustRegister(NewSharesCollector(db, logger))
				mock.MatchExpectationsInOrder(false) // Prometheus runs collectors concurrently.
				healthy := prometheus.NewGauge(prometheus.GaugeOpts{Name: "openstack_nova_up", Help: "up"})
				healthy.Set(1)
				reg.MustRegister(healthy)
				response := httptest.NewRecorder()
				promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				require.Equal(t, http.StatusOK, response.Code)
				require.Contains(t, response.Body.String(), "openstack_sharev2_quota_up 0\n")
				require.Contains(t, response.Body.String(), "openstack_sharev2_up 1\n")
				require.Contains(t, response.Body.String(), "openstack_sharev2_shares_counter 0\n")
				require.Contains(t, response.Body.String(), "openstack_nova_up 1\n")
				require.NotContains(t, response.Body.String(), "openstack_sharev2_quota_shares")
				require.NotContains(t, response.Body.String(), "openstack_sharev2_share_type_quota_")
				require.Contains(t, logs.String(), "Failed to collect manila quotas")
				require.NotContains(t, logs.String(), "secret")
				require.NotContains(t, logs.String(), "private-host")
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestQuotasCollectorEmptyAndInvalidDefaults(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		defaults := QuotaDefaults{}
		if !invalid {
			defaults = nil
			expectQuotaRows(mock, emptyQuotaRows())
		}
		reg := prometheus.NewPedanticRegistry()
		reg.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), defaults))
		families, err := reg.Gather()
		require.NoError(t, err)
		require.Len(t, families, 1)
		require.Equal(t, "openstack_sharev2_quota_up", families[0].GetName())
		want := float64(1)
		if invalid {
			want = 0
		}
		require.Equal(t, want, families[0].Metric[0].GetGauge().GetValue())
		require.NoError(t, mock.ExpectationsWereMet())
		mock.ExpectClose()
		require.NoError(t, db.Close())
	}
}

func TestQuotaConnectionFailureAndTimeoutConfig(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	resolver := project.NewResolver(logger, nil, 0)
	reg := prometheus.NewPedanticRegistry()
	RegisterCollectors(reg, "unsupported://private-user:secret@private-host/db", resolver, nil, logger, 0)
	families, err := reg.Gather()
	require.NoError(t, err)
	require.Len(t, families, 2)
	for _, f := range families {
		require.Contains(t, []string{"openstack_sharev2_up", "openstack_sharev2_quota_up"}, f.GetName())
		require.EqualValues(t, 0, f.Metric[0].GetGauge().GetValue())
	}
	require.NotContains(t, logs.String(), "secret")
	require.NotContains(t, logs.String(), "private-host")
	require.Equal(t, 5*time.Second, NewQuotasCollector(nil, logger, resolver, nil).queryTimeout)
	require.Equal(t, 5*time.Second, NewQuotasCollector(nil, logger, resolver, nil, -time.Second).queryTimeout)
	require.Equal(t, time.Second, NewQuotasCollector(nil, logger, resolver, nil, time.Second).queryTimeout)
	reg = prometheus.NewPedanticRegistry()
	RegisterCollectors(reg, "", resolver, nil, logger, 0)
	families, err = reg.Gather()
	require.NoError(t, err)
	require.Empty(t, families)
}

func TestQuotasConcurrentScrapes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.MatchExpectationsInOrder(false)
	for range 4 {
		expectQuotaRows(mock, emptyQuotaRows())
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), nil))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, err := reg.Gather()
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotaErrorLogging(t *testing.T) {
	for _, tc := range []struct {
		err            error
		classification string
	}{
		{fmt.Errorf("private-password: %w", context.DeadlineExceeded), "deadline exceeded"},
		{fmt.Errorf("private-password: %w", context.Canceled), "canceled"},
		{&mysql.MySQLError{Number: 1045, Message: "private-password"}, "mysql_error_code=1045"},
	} {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		c := NewQuotasCollector(nil, logger, project.NewResolver(logger, nil, 0), nil)
		ch := make(chan prometheus.Metric, 1)
		c.fail(ch, "query", tc.err)
		require.Len(t, ch, 1)
		require.Contains(t, logs.String(), tc.classification)
		require.NotContains(t, logs.String(), "private-password")
	}
}

// Invalid persisted values suppress scopes, never substitute a default limit.
func TestQuotasRejectInvalidRecordedLimits(t *testing.T) {
	for _, index := range []int{0, 1, 4} {
		for _, value := range []any{nil, -2} {
			t.Run(fmt.Sprintf("query=%d/value=%v", index, value), func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer func() { _ = db.Close() }()
				rows := emptyQuotaRows()
				rows[1].AddRow("healthy", "shares", 23)
				rows[3].AddRow("healthy", nil, "shares", 7, 2)
				rows[4].AddRow("healthy", "gold", "gold", "shares", 11)
				rows[5].AddRow("healthy", "gold", "gold", nil, "shares", 4, 1)
				rows[3].AddRow("affected", nil, "shares", 3, 1)
				rows[4].AddRow("affected", "silver", "silver", "shares", 9)
				switch index {
				case 0:
					rows[0].AddRow("shares", value)
				case 1:
					rows[1].AddRow("affected", "shares", value)
				case 4:
					rows[4].AddRow("affected", "gold", "gold", "shares", value)
				}
				expectQuotaRows(mock, rows)
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				reg := prometheus.NewPedanticRegistry()
				reg.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), nil))
				families, err := reg.Gather()
				require.NoError(t, err)
				counts := map[string]int{}
				for _, family := range families {
					if family.GetName() == "openstack_sharev2_quota_up" {
						require.EqualValues(t, 0, family.Metric[0].GetGauge().GetValue())
						continue
					}
					for _, metric := range family.Metric {
						labels := map[string]string{}
						for _, label := range metric.Label {
							labels[label.GetName()] = label.GetValue()
						}
						scope := labels["tenant_id"] + "/" + labels["share_type"]
						counts[scope]++
						if family.GetName() == "openstack_sharev2_quota_shares" && labels["tenant_id"] == "healthy" {
							require.Equal(t, map[string]float64{"limit": 23, "in_use": 7, "reserved": 2}[labels["type"]], metric.GetGauge().GetValue())
						}
						if family.GetName() == "openstack_sharev2_share_type_quota_shares" && labels["tenant_id"] == "healthy" {
							require.Equal(t, map[string]float64{"limit": 11, "in_use": 4, "reserved": 1}[labels["type"]], metric.GetGauge().GetValue())
						}
					}
				}
				if index == 0 {
					require.Len(t, families, 1) // Shared defaults fail closed for all projects.
					require.Empty(t, counts)
				} else {
					expected := map[string]int{"healthy/": 34, "healthy/gold": 18}
					if index == 4 { // A bad type leaves its project and sibling type valid.
						expected["affected/"] = 34
						expected["affected/silver"] = 18
					}
					require.Equal(t, expected, counts)
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

// Keep the public contract independent of the collector's resource table.
func TestQuotasMetricContracts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for range 2 {
		rows := emptyQuotaRows()
		rows[1].AddRow("p", "shares", 20)
		rows[4].AddRow("p", "t", "gold", "shares", 10)
		expectQuotaRows(mock, rows)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), nil))
	names := []string{
		"quota_shares", "quota_bytes", "quota_snapshots", "quota_snapshot_bytes",
		"quota_share_networks", "quota_share_replicas", "quota_replica_bytes",
		"quota_share_groups", "quota_share_group_snapshots", "quota_backups",
		"quota_backup_bytes", "quota_per_share_bytes",
		"share_type_quota_shares", "share_type_quota_bytes", "share_type_quota_snapshots",
		"share_type_quota_snapshot_bytes", "share_type_quota_share_replicas", "share_type_quota_replica_bytes",
		"quota_up",
	}
	for range 2 {
		families, err := registry.Gather()
		require.NoError(t, err)
		gotNames := make([]string, 0, len(families))
		for _, family := range families {
			name := strings.TrimPrefix(family.GetName(), "openstack_sharev2_")
			gotNames = append(gotNames, name)
			require.Equal(t, dto.MetricType_GAUGE, family.GetType(), name)
			if name == "quota_up" {
				require.Len(t, family.Metric, 1)
				require.Empty(t, family.Metric[0].Label)
				require.EqualValues(t, 1, family.Metric[0].GetGauge().GetValue())
				continue
			}
			labels := []string{"tenant", "tenant_id", "domain_id", "type"}
			if strings.HasPrefix(name, "share_type_") {
				labels = append(labels, "share_type", "share_type_name")
			}
			var states []string
			for _, metric := range family.Metric {
				var keys []string
				for _, label := range metric.Label {
					keys = append(keys, label.GetName())
					if label.GetName() == "type" {
						states = append(states, label.GetValue())
					}
				}
				require.ElementsMatch(t, labels, keys, name)
				require.NotNil(t, metric.Gauge)
			}
			if name == "quota_per_share_bytes" {
				require.Equal(t, []string{"limit"}, states)
			} else {
				require.ElementsMatch(t, []string{"limit", "in_use", "reserved"}, states, name)
			}
		}
		require.ElementsMatch(t, names, gotNames)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotasRawAccounting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typeName driver.Value
		included bool
	}{
		{"active named type", "gold", true},
		{"active empty name", "", true},
		{"active SQL NULL name", nil, false},
		{"deleted type", nil, false},
		{"missing type", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), nil))
			var previous map[string]float64
			for scrape := range 2 {
				// The query scopes overlap logically; only project rows contribute
				// to project totals. NULL components must be zero independently.
				data := [][][]driver.Value{
					{{"shares", 80}},
					{{"p", "shares", 20}},
					{{"p", "u1", "shares", 1}, {"user-only", "u2", "shares", 1}},
					{
						{"p", "u1", "shares", 2, 1}, {"p", "u2", "shares", 3, 2}, {"p", nil, "shares", -7, -5},
						{"p", "u1", "gigabytes", 2147483647, -2147483648}, {"p", "u2", "gigabytes", 2147483647, -2147483648},
						{"p", "u1", "snapshots", nil, 5}, {"p", "u2", "snapshots", -3, nil},
						{"p", nil, "backups", nil, nil}, {"null-only", nil, "backups", nil, nil},
						{nil, "u1", "shares", 999, 999}, {"", "u1", "shares", 999, 999},
					},
					{
						{"p", "t", tc.typeName, "shares", 10},
						// Excluded type metadata must not discover an owner or fail
						// validation of an otherwise healthy collection.
						{"ignored-limit-only", "gone", nil, "shares", -2},
					},
					{
						{"p", "t", tc.typeName, "u1", "shares", 4, 2}, {"p", "t", tc.typeName, "u2", "shares", -1, nil},
						{"p", "t", tc.typeName, nil, "shares", nil, -3},
						{"p", "t", tc.typeName, "u1", "gigabytes", 2147483647, -2147483648},
						{"p", "t", tc.typeName, "u2", "gigabytes", 2147483647, -2147483648},
						{"p", "t", tc.typeName, nil, "snapshots", nil, nil},
						{"ignored-usage-only", "gone", nil, nil, "shares", 999, 999},
						{nil, "t", "gold", nil, "shares", 999, 999}, {"", "t", "gold", nil, "shares", 999, 999},
						{"p", nil, "gold", nil, "shares", 999, 999}, {"p", "", "gold", nil, "shares", 999, 999},
					},
				}
				rows := emptyQuotaRows()
				for i, records := range data {
					if scrape == 1 {
						slices.Reverse(records)
					}
					for _, record := range records {
						rows[i].AddRow(record...)
					}
				}
				expectQuotaRows(mock, rows)
				families, err := registry.Gather()
				require.NoError(t, err)
				samples := map[string]float64{}
				for _, family := range families {
					for _, metric := range family.Metric {
						labels := map[string]string{}
						for _, label := range metric.Label {
							labels[label.GetName()] = label.GetValue()
						}
						if family.GetName() != "openstack_sharev2_quota_up" {
							require.NotContains(t, []string{"ignored-limit-only", "ignored-usage-only", ""}, labels["tenant_id"], family.GetName())
						}
						samples[strings.Join([]string{family.GetName(), labels["tenant_id"], labels["share_type"], labels["type"]}, "/")] = metric.GetGauge().GetValue()
						if strings.HasPrefix(family.GetName(), "openstack_sharev2_share_type_quota_") {
							require.Equal(t, tc.typeName, labels["share_type_name"])
						}
					}
				}
				for key, value := range map[string]float64{
					"quota_up///":           1,
					"quota_shares/p//limit": 20, "quota_shares/p//in_use": -2, "quota_shares/p//reserved": -2,
					"quota_shares/user-only//limit": 80,
					"quota_bytes/p//in_use":         float64(4294967294) * (1 << 30),
					"quota_bytes/p//reserved":       float64(-4294967296) * (1 << 30),
					"quota_snapshots/p//in_use":     -3, "quota_snapshots/p//reserved": 5,
					"quota_backups/p//in_use": 0, "quota_backups/p//reserved": 0,
					"quota_backups/null-only//in_use": 0, "quota_backups/null-only//reserved": 0,
				} {
					require.Contains(t, samples, "openstack_sharev2_"+key)
					require.Equal(t, value, samples["openstack_sharev2_"+key], key)
				}
				if tc.included {
					require.Len(t, families, 19)
					for key, value := range map[string]float64{
						"shares/p/t/limit": 10, "shares/p/t/in_use": 3, "shares/p/t/reserved": -1,
						"bytes/p/t/in_use":     float64(4294967294) * (1 << 30),
						"bytes/p/t/reserved":   float64(-4294967296) * (1 << 30),
						"snapshots/p/t/in_use": 0, "snapshots/p/t/reserved": 0,
					} {
						require.Contains(t, samples, "openstack_sharev2_share_type_quota_"+key)
						require.Equal(t, value, samples["openstack_sharev2_share_type_quota_"+key], key)
					}
				} else {
					require.Len(t, families, 13) // Project quotas plus healthy collection; no phantom type scopes.
				}
				if previous != nil {
					require.Equal(t, previous, samples, "reversed raw rows or repeated scrape changed accounting")
				}
				previous = samples
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
