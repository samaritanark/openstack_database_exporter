package octavia

import (
	"bytes"
	"database/sql"
	"errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	keystonedb "github.com/vexxhost/openstackdb/keystone/db"
	octaviadb "github.com/vexxhost/openstackdb/octavia/db/repositories"
)

var limitColumns = []string{"project_id", "health_monitor", "listener", "load_balancer", "member", "pool", "l7policy", "l7rule",
	"in_use_health_monitor", "in_use_listener", "in_use_load_balancer", "in_use_member", "in_use_pool", "in_use_l7policy", "in_use_l7rule"}
var usageColumns = []string{"project_id", "resource", "in_use"}

func testDefaults() QuotaDefaults {
	return QuotaDefaults{"loadbalancer": 10, "listener": 20, "pool": 30, "member": 40, "healthmonitor": 50, "l7policy": 60, "l7rule": 70}
}

func TestQuotasCollector(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metaDB, metaMock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() {
		metaMock.ExpectClose()
		if err := metaDB.Close(); err != nil {
			t.Errorf("close metaDB: %v", err)
		}
	}()
	metaMock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).
			AddRow("p1", "partner", "", 1, "domain-1", "", 0, "").
			AddRow("idle", "idle-partner", "", 1, "domain-2", "", 0, ""))
	resolver := project.NewResolver(logger, keystonedb.New(metaDB), time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(
		sqlmock.NewRows(limitColumns).
			AddRow("p1", 11, 0, 20, -1, nil, 33, 44, 999, 999, 999, 999, 999, 999, 999).
			AddRow("empty-limits", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	usage := sqlmock.NewRows(usageColumns).
		AddRow("p1", "load_balancer", 1).
		AddRow("p1", "listener", 2).
		AddRow("p1", "pool", 3).
		AddRow("p1", "member", 4).
		AddRow("p1", "health_monitor", 5).
		AddRow("p1", "l7policy", 6).
		AddRow("p1", "l7rule", 7).
		AddRow("usage-only", "member", 8)
	mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillReturnRows(usage)
	defaults := testDefaults()
	collector := NewQuotasCollector(db, logger, resolver, defaults)
	// Mutation by a caller must not change an already registered collector.
	defaults["member"] = 999
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 8)
	var exposition bytes.Buffer
	for _, family := range families {
		_, err := expfmt.MetricFamilyToText(&exposition, family)
		require.NoError(t, err)
	}
	require.Contains(t, exposition.String(), `openstack_loadbalancer_quota_member{domain_id="domain-1",tenant="partner",tenant_id="p1",type="in_use"} 4`)
	require.Contains(t, exposition.String(), `openstack_loadbalancer_quota_member{domain_id="domain-1",tenant="partner",tenant_id="p1",type="limit"} -1`)
	require.NotContains(t, exposition.String(), `type="reserved"`)
	limits := map[string]float64{"loadbalancer": 20, "listener": 0, "pool": 30, "member": -1, "healthmonitor": 11, "l7policy": 33, "l7rule": 44}
	used := map[string]float64{"loadbalancer": 1, "listener": 2, "pool": 3, "member": 4, "healthmonitor": 5, "l7policy": 6, "l7rule": 7}
	for _, family := range families {
		if family.GetName() == "openstack_loadbalancer_quota_up" {
			require.Equal(t, float64(1), family.Metric[0].GetGauge().GetValue())
			continue
		}
		require.Equal(t, "GAUGE", family.GetType().String())
		resource := family.GetName()[len("openstack_loadbalancer_quota_"):]
		require.Contains(t, limits, resource)
		require.Len(t, family.Metric, 8) // Two series per project, four projects.
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			require.Len(t, labels, 4)
			require.Contains(t, labels, "domain_id")
			pid := labels["tenant_id"]
			require.Contains(t, []string{"p1", "idle", "empty-limits", "usage-only"}, pid)
			require.Contains(t, []string{"limit", "in_use"}, labels["type"])
			expected := float64(0)
			if labels["type"] == "limit" {
				expected = float64(testDefaults()[resource])
				if pid == "p1" {
					expected = limits[resource]
				}
			}
			if labels["type"] == "in_use" {
				if pid == "p1" {
					expected = used[resource]
				}
				if pid == "usage-only" && resource == "member" {
					expected = 8
				}
			}
			require.Equal(t, expected, metric.GetGauge().GetValue(), "%s %v", resource, labels)
			switch pid {
			case "p1":
				require.Equal(t, "partner", labels["tenant"])
				require.Equal(t, "domain-1", labels["domain_id"])
			case "idle":
				require.Equal(t, "idle-partner", labels["tenant"])
				require.Equal(t, "domain-2", labels["domain_id"])
			default:
				require.Equal(t, pid, labels["tenant"])
				require.Empty(t, labels["domain_id"])
			}
		}
	}
	require.NoError(t, mock.ExpectationsWereMet())
	require.NoError(t, metaMock.ExpectationsWereMet())
}

func TestQuotasCollectorFailures(t *testing.T) {
	for _, scenario := range []string{"limits query", "usage query", "invalid defaults", "limits timeout", "usage timeout", "limits scan", "usage scan", "usage iteration", "shared deadline"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close db: %v", err)
				}
			}()
			mock.MatchExpectationsInOrder(false)
			db.SetMaxOpenConns(1)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			defaults := testDefaults()
			timeout := 20 * time.Millisecond
			switch scenario {
			case "limits query":
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnError(sql.ErrConnDone)
			case "usage query":
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns).AddRow("p1", 1, 1, 1, 1, 1, 1, 1, nil, nil, nil, nil, nil, nil, nil))
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillReturnError(sql.ErrConnDone)
			case "limits timeout":
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillDelayFor(time.Second).WillReturnRows(sqlmock.NewRows(limitColumns))
			case "usage timeout":
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns))
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillDelayFor(time.Second).WillReturnRows(sqlmock.NewRows(usageColumns))
			case "limits scan":
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns).
					AddRow("p1", "invalid integer", 1, 1, 1, 1, 1, 1, nil, nil, nil, nil, nil, nil, nil))
			case "usage scan", "usage iteration":
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns))
				rows := sqlmock.NewRows(usageColumns)
				if scenario == "usage scan" {
					rows.AddRow("p1", "member", "invalid integer")
				} else {
					rows.AddRow("p1", "member", 1).RowError(0, sql.ErrConnDone)
				}
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillReturnRows(rows)
			case "shared deadline":
				timeout = 250 * time.Millisecond
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillDelayFor(150 * time.Millisecond).WillReturnRows(sqlmock.NewRows(limitColumns))
				mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillDelayFor(150 * time.Millisecond).WillReturnRows(sqlmock.NewRows(usageColumns))
			case "invalid defaults":
				defaults = QuotaDefaults{}
			}
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), defaults, timeout))
			mock.ExpectQuery(regexp.QuoteMeta(octaviadb.LoadBalancerGetAllWithVIP)).WillReturnRows(
				sqlmock.NewRows([]string{"id", "project_id", "name", "provisioning_status", "operating_status", "provider", "vip_address"}).
					AddRow("lb-1", "p1", "web-lb", "ACTIVE", "ONLINE", "octavia", "203.0.113.50"))
			registry.MustRegister(NewLoadBalancerCollector(db, logger))
			healthy := prometheus.NewGauge(prometheus.GaugeOpts{Name: "openstack_nova_up", Help: "up"})
			healthy.Set(1)
			registry.MustRegister(healthy)
			response := httptest.NewRecorder()
			promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			require.Equal(t, http.StatusOK, response.Code)
			require.Contains(t, response.Body.String(), "openstack_nova_up 1\n")
			require.Contains(t, response.Body.String(), "openstack_loadbalancer_up 1\n")
			require.Contains(t, response.Body.String(), "openstack_loadbalancer_total_loadbalancers 1\n")
			require.Contains(t, response.Body.String(), `openstack_loadbalancer_loadbalancer_status{id="lb-1",name="web-lb",operating_status="ONLINE",project_id="p1",provider="octavia",provisioning_status="ACTIVE",vip_address="203.0.113.50"} 0`)

			require.Contains(t, response.Body.String(), "openstack_loadbalancer_quota_up 0\n")
			for _, resource := range octaviaQuotaResources {
				require.NotContains(t, response.Body.String(), "openstack_loadbalancer_quota_"+resource.metric)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotasCollectorMissingMetadata(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "not configured", true: "query unavailable"}[unavailable], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close db: %v", err)
				}
			}()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			var meta *keystonedb.Queries
			if unavailable {
				metaDB, metaMock, err := sqlmock.New()
				require.NoError(t, err)
				defer func() {
					metaMock.ExpectClose()
					if err := metaDB.Close(); err != nil {
						t.Errorf("close metaDB: %v", err)
					}
				}()
				meta = keystonedb.New(metaDB)
				// Initial load and stale-cache retry both fail.
				metaMock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnError(sql.ErrConnDone)
				metaMock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnError(sql.ErrConnDone)
				defer func() { require.NoError(t, metaMock.ExpectationsWereMet()) }()
			}
			resolver := project.NewResolver(logger, meta, time.Hour)
			mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns))
			mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillReturnRows(sqlmock.NewRows(usageColumns).AddRow("orphan", "member", 2))
			registry := prometheus.NewRegistry()
			registry.MustRegister(NewQuotasCollector(db, logger, resolver, testDefaults()))
			families, err := registry.Gather()
			require.NoError(t, err)
			require.Len(t, families, 8)
			for _, family := range families {
				if family.GetName() == "openstack_loadbalancer_quota_up" {
					continue
				}
				require.Len(t, family.Metric, 2)
				for _, metric := range family.Metric {
					labels := map[string]string{}
					for _, label := range metric.Label {
						labels[label.GetName()] = label.GetValue()
					}
					require.Equal(t, "orphan", labels["tenant"])
					require.Equal(t, "orphan", labels["tenant_id"])
					require.Empty(t, labels["domain_id"])
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotasCollectorEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns))
	mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillReturnRows(sqlmock.NewRows(usageColumns))
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), testDefaults()))
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	require.Equal(t, "openstack_loadbalancer_quota_up", families[0].GetName())
	require.Equal(t, float64(1), families[0].Metric[0].GetGauge().GetValue())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotasCollectorUnknownResourceAndTimeout(t *testing.T) {
	for _, scenario := range []string{"unknown resource", "timeout", "safe logging", "mysql safe logging"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close db: %v", err)
				}
			}()
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns).AddRow("p1", 1, 1, 1, 1, 1, 1, 1, nil, nil, nil, nil, nil, nil, nil))
			query := mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll))
			switch scenario {
			case "unknown resource":
				query.WillReturnRows(sqlmock.NewRows(usageColumns).AddRow("p1", "member", 4).AddRow("p1", "healthmonitor", 1))
			case "timeout":
				query.WillDelayFor(time.Second).WillReturnRows(sqlmock.NewRows(usageColumns))
			case "safe logging":
				query.WillReturnError(errors.New("mysql://private-user:secret@private-host/db"))
			case "mysql safe logging":
				query.WillReturnError(&mysql.MySQLError{Number: 1045, Message: "mysql://private-user:secret@private-host/db"})
			}
			collector := NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), testDefaults())
			if scenario == "timeout" {
				collector.queryTimeout = 20 * time.Millisecond
			}
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(collector)
			families, err := registry.Gather()
			require.NoError(t, err)
			require.Len(t, families, 1)
			require.Equal(t, "openstack_loadbalancer_quota_up", families[0].GetName())
			require.Equal(t, float64(0), families[0].Metric[0].GetGauge().GetValue())
			require.Contains(t, logs.String(), "Failed to collect octavia quotas")
			require.NotContains(t, logs.String(), "secret")
			require.NotContains(t, logs.String(), "private-host")
			if scenario == "mysql safe logging" {
				require.Contains(t, logs.String(), "mysql_error_code=1045")
				require.Contains(t, logs.String(), "operation=\"query usage\"")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotasCollectorWithoutDefaultsFile(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "programmatic nil", true: "no file"}[configured], func(t *testing.T) {
			var defaults QuotaDefaults
			if configured {
				var err error
				defaults, err = LoadQuotaDefaults("")
				require.NoError(t, err)
			}
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close db: %v", err)
				}
			}()
			mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaGetAll)).WillReturnRows(sqlmock.NewRows(limitColumns).
				AddRow("p1", nil, 0, nil, -1, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
			mock.ExpectQuery(regexp.QuoteMeta(octaviadb.QuotaUsageGetAll)).WillReturnRows(sqlmock.NewRows(usageColumns).AddRow("usage-only", "member", 2))
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), defaults))
			families, err := registry.Gather()
			require.NoError(t, err)
			require.Len(t, families, 8)
			for _, family := range families {
				if family.GetName() == "openstack_loadbalancer_quota_up" {
					require.EqualValues(t, 1, family.Metric[0].GetGauge().GetValue())
					continue
				}
				for _, metric := range family.Metric {
					labels := map[string]string{}
					for _, label := range metric.Label {
						labels[label.GetName()] = label.GetValue()
					}
					if labels["type"] == "limit" {
						want := float64(-1)
						if labels["tenant_id"] == "p1" && family.GetName() == "openstack_loadbalancer_quota_listener" {
							want = 0
						}
						require.Equal(t, want, metric.GetGauge().GetValue())
					}
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotaSuccessGaugeOnConnectionFailure(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	registry := prometheus.NewPedanticRegistry()
	RegisterCollectors(registry, "unsupported://user:secret@private-host/db", project.NewResolver(logger, nil, 0), nil, logger, 0)
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 2)
	for _, family := range families {
		require.Contains(t, []string{"openstack_loadbalancer_up", "openstack_loadbalancer_quota_up"}, family.GetName())
		require.EqualValues(t, 0, family.Metric[0].GetGauge().GetValue())
	}
	require.NotContains(t, logs.String(), "secret")
	require.NotContains(t, logs.String(), "private-host")
}

func TestQuotaQueryTimeoutConfiguration(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver := project.NewResolver(logger, nil, 0)
	require.Equal(t, 5*time.Second, NewQuotasCollector(nil, logger, resolver, testDefaults()).queryTimeout)
	require.Equal(t, 5*time.Second, NewQuotasCollector(nil, logger, resolver, testDefaults(), 0).queryTimeout)
	require.Equal(t, 4*time.Second, NewQuotasCollector(nil, logger, resolver, testDefaults(), 4*time.Second).queryTimeout)
}
