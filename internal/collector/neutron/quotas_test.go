package neutron

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	keystonedb "github.com/vexxhost/openstackdb/keystone/db"
	neutrondb "github.com/vexxhost/openstackdb/neutron/db"
)

func quotaRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"project_id", "resource", "limit"}) }
func usageRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"project_id", "resource", "cnt"}) }
func reservedRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"reservation_id", "project_id", "expiration", "resource", "amount"})
}
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func expectQuotaReads(mock sqlmock.Sqlmock, limits, usage, reserved *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetQuotas)).WillReturnRows(limits)
	mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetResourceCountsByProject)).WillReturnRows(usage)
	mock.ExpectQuery(regexp.QuoteMeta(reservationQuery())).WithArgs(reservationArgs()...).WillReturnRows(reserved)
}
func reservationQuery() string {
	return strings.Replace(neutrondb.GetReservationsForResources, "/*SLICE:resources*/?", strings.TrimSuffix(strings.Repeat("?,", len(quotaResources)), ","), 1)
}
func reservationArgs() []driver.Value {
	args := make([]driver.Value, len(quotaResources))
	for i, resource := range quotaResources {
		args[i] = resource
	}
	return args
}
func gatherQuota(t *testing.T, c *QuotaCollector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(c))
	families, err := reg.Gather()
	require.NoError(t, err)
	result := map[string]*dto.MetricFamily{}
	for _, f := range families {
		result[f.GetName()] = f
	}
	return result
}
func labels(m *dto.Metric) map[string]string {
	result := map[string]string{}
	for _, p := range m.Label {
		result[p.GetName()] = p.GetValue()
	}
	return result
}
func TestQuotaAccountingAndIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	kd, km, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { km.ExpectClose(); require.NoError(t, kd.Close()) })
	km.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).AddRow("a", "shared", "", true, "domain-a", "", false, "").AddRow("b", "shared", "", true, "domain-b", "", false, "").AddRow("idle", "", "", true, "domain-c", "", false, ""))
	resolver := project.NewResolver(testLogger(), keystonedb.New(kd), time.Hour)
	limits, usage, reserved := quotaRows(), usageRows(), reservedRows()
	defaults := upstreamQuotaDefaults()
	for i, r := range requiredResources {
		defaults[r] = int32(70 + i)
		limits.AddRow("a", r, i) // zero is a real override
		usage.AddRow("a", r, i+1)
		reserved.AddRow("r1", "a", time.Now().UTC(), r, i+2)
	}
	limits.AddRow("b", "port", -5) // recorded negative overrides pass through
	usage.AddRow("usage-only", "port", 9)
	reserved.AddRow("r2", "reservation-only", time.Now().UTC(), "router", 3)
	usage.AddRow("a", "subnet", 6).AddRow("a", "subnetpool", 2).AddRow("a", "rbac_policy", 4)
	expectQuotaReads(mock, limits, usage, reserved)
	families := gatherQuota(t, NewQuotaCollector(db, testLogger(), resolver, QuotaOptions{Defaults: defaults}))
	require.Equal(t, float64(1), families["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
	for _, r := range quotaResources {
		metrics := families["openstack_neutron_quota_"+r].Metric
		require.Len(t, metrics, 15) // five distinct owners, including metadata-only
		for _, m := range metrics {
			l := labels(m)
			require.Len(t, l, 4)
			require.NotEqual(t, "in_use", l["type"])
			id := l["tenant_id"]
			if id == "a" || id == "b" {
				require.Equal(t, "shared", l["tenant"])
				require.Equal(t, "domain-"+id, l["domain_id"])
			}
			if id == "idle" {
				require.Equal(t, "idle", l["tenant"])
				require.Equal(t, "domain-c", l["domain_id"])
			}
			if id == "usage-only" || id == "reservation-only" {
				require.Equal(t, id, l["tenant"])
				require.Empty(t, l["domain_id"])
			}
			expected := float64(0)
			switch l["type"] {
			case "limit":
				expected = float64(neutronDefaultQuotas[r])
				if isRequiredResource(r) {
					expected = float64(defaults[r])
				}
				if id == "a" {
					for i, resource := range requiredResources {
						if r == resource {
							expected = float64(i)
						}
					}
				}
				if id == "b" && r == "port" {
					expected = -5
				}
			case "used":
				if id == "a" {
					for i, resource := range requiredResources {
						if r == resource {
							expected = float64(i + 1)
						}
					}
					switch r {
					case "subnet":
						expected = 6
					case "subnetpool":
						expected = 2
					case "rbac_policy":
						expected = 4
					}
				}
				if id == "usage-only" && r == "port" {
					expected = 9
				}
			case "reserved":
				if id == "a" {
					for i, resource := range requiredResources {
						if r == resource {
							expected = float64(i + 2)
						}
					}
				}
				if id == "reservation-only" && r == "router" {
					expected = 3
				}
			default:
				t.Fatalf("unexpected measurement %q", l["type"])
			}
			require.Equal(t, expected, m.Gauge.GetValue(), "%s %v", r, l)
		}
	}
	require.NoError(t, mock.ExpectationsWereMet())
	require.NoError(t, km.ExpectationsWereMet())
}

func TestQuotaMalformedDataIsolated(t *testing.T) {
	tests := []struct {
		name                    string
		limits, usage, reserved *sqlmock.Rows
	}{
		{"null limit", quotaRows().AddRow("a", "port", nil), usageRows(), reservedRows()},
		{"duplicate limits", quotaRows().AddRow("a", "port", 0).AddRow("a", "port", 1), usageRows(), reservedRows()},
		{"empty project", quotaRows().AddRow("", "port", 1), usageRows(), reservedRows()},
		{"null resource", quotaRows().AddRow("a", nil, 1), usageRows(), reservedRows()},
		{"negative usage", quotaRows(), usageRows().AddRow("a", "port", -1), reservedRows()},
		{"duplicate usage", quotaRows(), usageRows().AddRow("a", "port", 1).AddRow("a", "port", 2), reservedRows()},
		{"negative reservation", quotaRows(), usageRows(), reservedRows().AddRow("r3", "a", time.Now().UTC(), "port", -1)},
		{"invalid then valid", quotaRows().AddRow("a", "port", nil).AddRow("a", "port", 12), usageRows(), reservedRows()},
		{"negative delta hidden by sum", quotaRows(), usageRows(), reservedRows().AddRow("hidden-negative", "a", time.Now().UTC(), "port", -2).AddRow("hidden-positive", "a", time.Now().UTC(), "port", 12)},
		{"null delta", quotaRows(), usageRows(), reservedRows().AddRow("r5", "a", time.Now().UTC(), "port", nil)},
		{"null owner", quotaRows(), usageRows(), reservedRows().AddRow("r6", nil, time.Now().UTC(), "port", 1)},
		{"whitespace reservation owner", quotaRows(), usageRows(), reservedRows().AddRow("space", " \t", time.Now().UTC(), "port", 1)},
		{"whitespace usage owner", quotaRows(), usageRows().AddRow(" \t", "port", 1), reservedRows()},
		{"null usage owner", quotaRows(), usageRows().AddRow(nil, "port", 40), reservedRows()},
		{"duplicate reservations", quotaRows(), usageRows(), reservedRows().AddRow("r7", "a", time.Now().UTC(), "port", 1).AddRow("r7", "a", time.Now().UTC(), "port", 2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
			tt.usage.AddRow("healthy", "network", 7).AddRow("a", "network", 3)
			expectQuotaReads(mock, tt.limits, tt.usage, tt.reserved)
			f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil))
			require.EqualValues(t, 1, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
			expectedSkipped := 1
			if tt.name == "invalid then valid" {
				expectedSkipped = 2
			}
			require.EqualValues(t, expectedSkipped, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
			for _, m := range f["openstack_neutron_quota_port"].GetMetric() {
				require.NotEqual(t, "a", labels(m)["tenant_id"])
			}
			if tt.name == "null owner" || tt.name == "null usage owner" {
				require.NotContains(t, f, "openstack_neutron_quota_port")
			}
			require.Contains(t, f, "openstack_neutron_quota_network")
			found := false
			for _, m := range f["openstack_neutron_quota_network"].GetMetric() {
				l := labels(m)
				if l["tenant_id"] == "healthy" && l["type"] == "used" {
					require.EqualValues(t, 7, m.Gauge.GetValue())
					found = true
				}
			}
			require.True(t, found)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotaReadFailures(t *testing.T) {
	queries := []string{neutrondb.GetQuotas, neutrondb.GetResourceCountsByProject, reservationQuery()}
	for stage := range queries {
		for _, timeout := range []bool{false, true} {
			t.Run(queries[stage][:30]+time.Duration(stage).String()+map[bool]string{true: "timeout", false: "error"}[timeout], func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
				for i := 0; i < stage; i++ {
					rows := quotaRows()
					if i == 1 {
						rows = usageRows()
					}
					mock.ExpectQuery(regexp.QuoteMeta(queries[i])).WillReturnRows(rows)
				}
				exp := mock.ExpectQuery(regexp.QuoteMeta(queries[stage]))
				if timeout {
					exp.WillDelayFor(time.Second).WillReturnRows(quotaRows())
				} else {
					exp.WillReturnError(errors.New("mysql://secret:password@host"))
				}
				f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil, QuotaOptions{QueryTimeout: 200 * time.Millisecond}))
				require.Len(t, f, 1)
				require.Zero(t, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestQuotaConfigurationFailuresAndDisconnectedRegistration(t *testing.T) {
	for _, opts := range []QuotaOptions{{Defaults: QuotaDefaults{}}, {QueryTimeout: -time.Second}, {ConfigError: sql.ErrConnDone}} {
		f := gatherQuota(t, NewQuotaCollector(nil, testLogger(), nil, opts))
		require.Len(t, f, 1)
		require.Zero(t, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
	}
	reg := prometheus.NewPedanticRegistry()
	RegisterCollectors(reg, "invalid-url", nil, testLogger())
	f, err := reg.Gather()
	require.NoError(t, err)
	names := map[string]float64{}
	for _, m := range f {
		names[m.GetName()] = m.Metric[0].Gauge.GetValue()
	}
	require.Contains(t, names, "openstack_neutron_quota_up")
	require.Zero(t, names["openstack_neutron_quota_up"])
	require.Contains(t, names, "openstack_neutron_up")
}

func TestEmptyQuotaCollection(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	expectQuotaReads(mock, quotaRows(), usageRows(), reservedRows())
	f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil))
	require.Len(t, f, 2)
	require.Zero(t, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
	require.Equal(t, float64(1), f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
	require.NoError(t, mock.ExpectationsWereMet())
}

// A quota failure must not suppress the independent inventory health metric.
func TestQuotaFailureWithInventory(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetQuotas)).WillReturnError(sql.ErrConnDone)
	mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetHARouterAgentPortBindingsWithAgents)).WillReturnRows(sqlmock.NewRows([]string{"router_id", "l3_agent_id", "state", "agent_host", "agent_admin_state_up", "agent_heartbeat_timestamp"}))
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewQuotaCollector(db, testLogger(), nil), NewHARouterAgentPortBindingCollector(db, testLogger()))
	families, err := reg.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, f := range families {
		values[f.GetName()] = f.Metric[0].Gauge.GetValue()
	}
	require.Equal(t, map[string]float64{"openstack_neutron_quota_up": 0, "openstack_neutron_up": 1}, values)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUnknownExtensionOwnerAndSanitizedErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	// Unsupported extension limits do not invalidate supported accounting.
	expectQuotaReads(mock, quotaRows().AddRow("extension-owner", "extension", nil), usageRows(), reservedRows())
	families := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil))
	require.Len(t, families, 11)
	require.Equal(t, "extension-owner", labels(families["openstack_neutron_quota_port"].Metric[0])["tenant_id"])
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetQuotas)).WillReturnError(errors.New("mysql://credential:password@host/database"))
	f := gatherQuota(t, NewQuotaCollector(db, logger, nil))
	require.Len(t, f, 1)
	require.NotContains(t, logs.String(), "credential")
	require.NotContains(t, logs.String(), "password")
	require.NotContains(t, logs.String(), "mysql://")
	require.Contains(t, logs.String(), "Failed to collect Neutron quotas")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMetadataReadFailureRetainsOwnerFallback(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	kd, km, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { km.ExpectClose(); require.NoError(t, kd.Close()) })
	km.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnError(sql.ErrConnDone)
	resolver := project.NewResolver(testLogger(), keystonedb.New(kd), time.Nanosecond)
	km.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnError(sql.ErrConnDone)
	expectQuotaReads(mock, quotaRows(), usageRows().AddRow("unknown", "port", 1), reservedRows())
	f := gatherQuota(t, NewQuotaCollector(db, testLogger(), resolver))
	require.Len(t, f, 11)
	require.Equal(t, float64(1), f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
	for _, m := range f["openstack_neutron_quota_port"].Metric {
		l := labels(m)
		require.Equal(t, "unknown", l["tenant"])
		require.Equal(t, "unknown", l["tenant_id"])
		require.Empty(t, l["domain_id"])
	}
	require.NoError(t, mock.ExpectationsWereMet())
	require.NoError(t, km.ExpectationsWereMet())
}

func TestQuotaAllNineFamiliesUseUsed(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	expectQuotaReads(mock, quotaRows(), usageRows().AddRow("owner", "port", 3), reservedRows())
	f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil))
	require.Len(t, quotaResources, 9)
	for _, r := range quotaResources {
		types := map[string]bool{}
		for _, m := range f["openstack_neutron_quota_"+r].Metric {
			types[labels(m)["type"]] = true
		}
		require.Equal(t, map[string]bool{"limit": true, "used": true, "reserved": true}, types, r)
		require.NotContains(t, types, "in_use")
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotaStaleAndSlowMetadata(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, slow := range []bool{false, true} {
			t.Run(fmt.Sprintf("cached=%t/slow=%t", cached, slow), func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
				kd, km, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() { km.ExpectClose(); require.NoError(t, kd.Close()) })
				initial := km.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects))
				if cached {
					initial.WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).AddRow("owner", "cached-name", "", true, "cached-domain", "", false, ""))
				} else {
					initial.WillReturnError(sql.ErrConnDone)
				}
				resolver := project.NewResolver(testLogger(), keystonedb.New(kd), time.Nanosecond)
				refresh := km.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects))
				if slow {
					refresh.WillDelayFor(5 * time.Second).WillReturnError(sql.ErrConnDone)
				} else {
					refresh.WillReturnError(sql.ErrConnDone)
				}
				expectQuotaReads(mock, quotaRows(), usageRows().AddRow("owner", "port", 7).AddRow("unknown", "network", 2), reservedRows())
				// Leave headroom for the required SQL reads under -race; only the
				// optional metadata query should exhaust the shared deadline.
				started := time.Now()
				f := gatherQuota(t, NewQuotaCollector(db, testLogger(), resolver, QuotaOptions{QueryTimeout: time.Second}))
				require.Less(t, time.Since(started), 3*time.Second)
				require.EqualValues(t, 1, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
				require.Len(t, f["openstack_neutron_quota_port"].Metric, 6)
				for _, m := range f["openstack_neutron_quota_port"].Metric {
					l := labels(m)
					name, domain := l["tenant_id"], ""
					if cached && l["tenant_id"] == "owner" {
						name, domain = "cached-name", "cached-domain"
					}
					require.Equal(t, name, l["tenant"])
					require.Equal(t, domain, l["domain_id"])
					if l["tenant_id"] == "owner" && l["type"] == "used" {
						require.EqualValues(t, 7, m.Gauge.GetValue())
					}
				}
				require.NoError(t, mock.ExpectationsWereMet())
				require.NoError(t, km.ExpectationsWereMet())
			})
		}
	}
}

func TestQuotaSecondaryDefaultsAndOverrides(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	defaults := upstreamQuotaDefaults()
	defaults["subnet"], defaults["subnetpool"], defaults["rbac_policy"] = 0, -10, 23
	limits := quotaRows().AddRow("override", "subnet", 14).AddRow("override", "subnetpool", 0).AddRow("override", "rbac_policy", -7)
	expectQuotaReads(mock, limits, usageRows().AddRow("default", "subnet", 2), reservedRows())
	f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil, QuotaOptions{Defaults: defaults}))
	for r, want := range map[string]float64{"subnet": 0, "subnetpool": -1, "rbac_policy": 23} {
		for _, m := range f["openstack_neutron_quota_"+r].Metric {
			l := labels(m)
			switch l["type"] {
			case "limit":
				expected := want
				if l["tenant_id"] == "override" {
					expected = map[string]float64{"subnet": 14, "subnetpool": 0, "rbac_policy": -7}[r]
				}
				require.Equal(t, expected, m.Gauge.GetValue())
			case "reserved":
				require.Zero(t, m.Gauge.GetValue())
			}
		}
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotaConfigFailureDoesNotLogOnScrapes(t *testing.T) {
	for _, tt := range []struct {
		opts   QuotaOptions
		reason string
	}{
		{QuotaOptions{Defaults: QuotaDefaults{}}, "missing key"},
		{QuotaOptions{QueryTimeout: -time.Second}, "query timeout must be positive"},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			c := NewQuotaCollector(nil, logger, nil, tt.opts)
			require.Contains(t, logs.String(), `"reason":"`+tt.reason+`"`)
			before := logs.String()
			for range 2 {
				f := gatherQuota(t, c)
				require.Len(t, f, 1)
				require.Zero(t, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
			}
			require.Equal(t, before, logs.String())
		})
	}
}

func TestQuotaConfigurationFailureWithInventory(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetHARouterAgentPortBindingsWithAgents)).WillReturnRows(sqlmock.NewRows([]string{"router_id", "l3_agent_id", "state", "agent_host", "agent_admin_state_up", "agent_heartbeat_timestamp"}))
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewQuotaCollector(db, testLogger(), nil, QuotaOptions{Defaults: QuotaDefaults{}}), NewHARouterAgentPortBindingCollector(db, testLogger()))
	families, err := reg.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, f := range families {
		values[f.GetName()] = f.Metric[0].Gauge.GetValue()
	}
	require.Equal(t, map[string]float64{"openstack_neutron_quota_up": 0, "openstack_neutron_up": 1}, values)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotaSecondaryReservationsAndExtensions(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	reserved := reservedRows().AddRow("r9", "a", time.Now().UTC(), "extension-secret", nil)
	for _, r := range quotaResources {
		reserved.AddRow("r10", "a", time.Now().UTC(), r, 5).AddRow("r11", "b", time.Now().UTC(), r, 9)
	}
	expectQuotaReads(mock, quotaRows().AddRow("a", "subnetpool", -1), usageRows().AddRow("a", "port", 11).AddRow("a", "network", 6), reserved)
	var logs bytes.Buffer
	f := gatherQuota(t, NewQuotaCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), nil))
	require.EqualValues(t, 1, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
	require.Zero(t, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
	for _, r := range quotaResources {
		require.Len(t, f["openstack_neutron_quota_"+r].Metric, 6)
		for _, m := range f["openstack_neutron_quota_"+r].Metric {
			l := labels(m)
			if l["type"] == "reserved" {
				expected := 5
				if l["tenant_id"] == "b" {
					expected = 9
				}
				require.EqualValues(t, expected, m.Gauge.GetValue())
			}
			if l["tenant_id"] == "a" && l["type"] == "used" && r == "port" {
				require.EqualValues(t, 11, m.Gauge.GetValue())
			}
			if l["tenant_id"] == "a" && l["type"] == "used" && r == "network" {
				require.EqualValues(t, 6, m.Gauge.GetValue())
			}
			if l["tenant_id"] == "a" && l["type"] == "limit" && r == "subnetpool" {
				require.EqualValues(t, -1, m.Gauge.GetValue())
			}
		}
	}
	require.NotContains(t, logs.String(), "extension-secret")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotaUnidentifiableScopesStayOmitted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limits   *sqlmock.Rows
		reserved *sqlmock.Rows
		omitted  func(string, string) bool
	}{
		{"missing owner", quotaRows(), reservedRows().AddRow("r12", nil, time.Now().UTC(), "port", 2), func(_ string, r string) bool { return r == "port" }},
		{"missing resource", quotaRows().AddRow("a", nil, 3), reservedRows(), func(p, _ string) bool { return p == "a" }},
		{"both missing", quotaRows().AddRow(nil, nil, 3), reservedRows(), func(_, _ string) bool { return true }},
		{"reservation invalid then valid", quotaRows(), reservedRows().AddRow("r13", "a", time.Now().UTC(), "port", nil).AddRow("r14", "a", time.Now().UTC(), "port", 2), func(p, r string) bool { return p == "a" && r == "port" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
			expectQuotaReads(mock, tc.limits, usageRows().AddRow("a", "network", 3).AddRow("a", "port", 2).AddRow("b", "port", 7), tc.reserved)
			f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil))
			require.EqualValues(t, 1, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
			expectedSkipped := 1

			require.EqualValues(t, expectedSkipped, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
			for _, p := range []string{"a", "b"} {
				for _, r := range quotaResources {
					samples := 0
					for _, m := range f["openstack_neutron_quota_"+r].GetMetric() {
						if labels(m)["tenant_id"] == p {
							samples++
						}
					}
					expected := 3
					if tc.omitted(p, r) {
						expected = 0
					}
					require.Equal(t, expected, samples, "%s/%s", p, r)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotaSkippedGaugeResetsEachScrape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	c := NewQuotaCollector(db, testLogger(), nil)
	expectQuotaReads(mock, quotaRows().AddRow("a", "port", nil), usageRows(), reservedRows())
	f := gatherQuota(t, c)
	require.EqualValues(t, 1, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
	require.NotContains(t, f, "openstack_neutron_quota_port")
	expectQuotaReads(mock, quotaRows().AddRow("a", "port", 0), usageRows(), reservedRows())
	f = gatherQuota(t, c)
	require.Zero(t, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
	require.Len(t, f["openstack_neutron_quota_port"].Metric, 3)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Mocked aggregates deliberately bypass SQL filtering to exercise collector defense.
func TestQuotaSystemOwnedAggregates(t *testing.T) {
	for _, scenario := range []string{"reviewer usage", "reservations", "multiple projects", "clean"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
			usage := usageRows().AddRow("a", "port", 12).AddRow("a", "network", 3).AddRow("a", "subnet", 2)
			reserved := reservedRows().AddRow("r15", "a", time.Now().UTC(), "port", 4)
			projects := []string{"a"}
			if scenario == "reviewer usage" || scenario == "multiple projects" {
				usage.AddRow("", "port", 40).AddRow("", "network", 2).AddRow("", "subnet", 2)
			}
			if scenario == "reservations" || scenario == "multiple projects" {
				for _, r := range quotaResources {
					reserved.AddRow("r16", "", time.Now().UTC(), r, 40)
				}
			}
			if scenario == "multiple projects" {
				projects = append(projects, "b")
				usage.AddRow("b", "port", 7)
				reserved.AddRow("r17", "b", time.Now().UTC(), "network", 5)
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			c := NewQuotaCollector(db, logger, nil, QuotaOptions{Defaults: upstreamQuotaDefaults()})
			expectQuotaReads(mock, quotaRows(), usage, reserved)
			f := gatherQuota(t, c)
			require.EqualValues(t, 1, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
			require.Zero(t, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
			for _, r := range quotaResources {
				metrics := f["openstack_neutron_quota_"+r].GetMetric()
				require.Len(t, metrics, 3*len(projects), r)
				for _, m := range metrics {
					l := labels(m)
					p := l["tenant_id"]
					require.Contains(t, projects, p)
					require.Equal(t, p, l["tenant"])
					require.Empty(t, l["domain_id"])
					var expected float64
					switch l["type"] {
					case "limit":
						expected = float64(neutronDefaultQuotas[r])
					case "used":
						if p == "a" {
							expected = map[string]float64{"port": 12, "network": 3, "subnet": 2}[r]
						}
						if p == "b" && r == "port" {
							expected = 7
						}
					case "reserved":
						if p == "a" && r == "port" {
							expected = 4
						}
						if p == "b" && r == "network" {
							expected = 5
						}
					default:
						t.Fatalf("unexpected measurement %q", l["type"])
					}
					require.Equal(t, expected, m.Gauge.GetValue(), "%s %v", r, l)
				}
			}
			require.Empty(t, logs.String())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotaRawReservationDeltas(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	expectQuotaReads(mock, quotaRows(), usageRows(), reservedRows().
		AddRow("first", "p", time.Now().UTC(), "port", int32(2147483647)).
		AddRow("second", "p", time.Now().UTC().Add(time.Hour), "port", int32(2147483647)).
		AddRow("first", "p", time.Now().UTC(), "network", 3))
	f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil))
	for resource, want := range map[string]float64{"port": 4294967294, "network": 3} {
		for _, metric := range f["openstack_neutron_quota_"+resource].Metric {
			if labels(metric)["type"] == "reserved" {
				require.Equal(t, want, metric.Gauge.GetValue())
			}
		}
	}
	require.Zero(t, f["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotaConcurrentHTTP(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1) // Keep sqlmock connection lifecycle deterministic under concurrent scrapes.
	mock.MatchExpectationsInOrder(false)
	const scrapes = 8
	for range scrapes {
		expectQuotaReads(mock, quotaRows(), usageRows().AddRow("owner", "network", 2), reservedRows().AddRow("r", "owner", time.Now().UTC(), "network", 4))
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewQuotaCollector(db, testLogger(), nil))
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	results := make(chan *httptest.ResponseRecorder, scrapes)
	for range scrapes {
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			results <- response
		}()
	}
	for range scrapes {
		response := <-results
		require.Equal(t, http.StatusOK, response.Code)
		parser := expfmt.NewTextParser(model.UTF8Validation)
		families, err := parser.TextToMetricFamilies(response.Body)
		require.NoError(t, err)
		require.Len(t, families, 11)
		for _, resource := range quotaResources {
			family := families["openstack_neutron_quota_"+resource]
			require.Equal(t, dto.MetricType_GAUGE, family.GetType())
			require.Equal(t, "quota_"+resource, family.GetHelp())
			require.Len(t, family.Metric, 3)
			for _, metric := range family.Metric {
				l := labels(metric)
				require.Equal(t, map[string]string{"tenant": "owner", "tenant_id": "owner", "domain_id": "", "type": l["type"]}, l)
				want := float64(neutronDefaultQuotas[resource])
				switch l["type"] {
				case "used":
					want = 0
					if resource == "network" {
						want = 2
					}
				case "reserved":
					want = 0
					if resource == "network" {
						want = 4
					}
				default:
					require.Equal(t, "limit", l["type"])
				}
				require.Equal(t, want, metric.Gauge.GetValue())
			}
		}
		require.EqualValues(t, 1, families["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
		require.Zero(t, families["openstack_neutron_quota_skipped_records"].Metric[0].Gauge.GetValue())
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQuotaReservationScanAndIterationFailures(t *testing.T) {
	for _, kind := range []string{"amount", "expiration", "iteration"} {
		t.Run(kind, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
			mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetQuotas)).WillReturnRows(quotaRows())
			mock.ExpectQuery(regexp.QuoteMeta(neutrondb.GetResourceCountsByProject)).WillReturnRows(usageRows().AddRow("owner", "network", 1))
			var amount, expiration any = 1, time.Now().UTC()
			if kind == "amount" {
				amount = "malformed"
			}
			if kind == "expiration" {
				expiration = "malformed"
			}
			rows := reservedRows().AddRow("first", "owner", time.Now().UTC(), "network", 3).AddRow("second", "owner", expiration, "network", amount)
			if kind == "iteration" {
				rows.RowError(1, errors.New("interrupted"))
			}
			mock.ExpectQuery(regexp.QuoteMeta(reservationQuery())).WithArgs(reservationArgs()...).WillReturnRows(rows).RowsWillBeClosed()
			f := gatherQuota(t, NewQuotaCollector(db, testLogger(), nil))
			require.Len(t, f, 1)
			require.Zero(t, f["openstack_neutron_quota_up"].Metric[0].Gauge.GetValue())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
