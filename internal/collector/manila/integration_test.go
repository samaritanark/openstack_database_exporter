//go:build integration

package manila

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	itest "github.com/vexxhost/openstack_database_exporter/internal/testutil"
)

func TestIntegration_SharesCollector(t *testing.T) {
	itest.SkipIfNoDocker(t)

	// prereqs.sql creates the share_groups stub table required by shares FK
	db := itest.NewMySQLContainer(t, "manila",
		"../../../sql/manila/prereqs.sql",
		"../../../sql/manila/schema.sql",
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("empty database", func(t *testing.T) {
		collector := NewSharesCollector(db, logger)

		// Should emit: up=1, shares_counter=0, and 19 status_counter metrics
		count := testutil.CollectAndCount(collector)
		// 1 up + 1 shares_counter + 19 share_status_counter = 21
		if count != 21 {
			t.Fatalf("expected 21 metrics for empty shares, got %d", count)
		}

		expected := `# HELP openstack_sharev2_shares_counter shares_counter
# TYPE openstack_sharev2_shares_counter gauge
openstack_sharev2_shares_counter 0
# HELP openstack_sharev2_up up
# TYPE openstack_sharev2_up gauge
openstack_sharev2_up 1
`
		err := testutil.CollectAndCompare(collector, strings.NewReader(expected),
			"openstack_sharev2_shares_counter",
			"openstack_sharev2_up",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("with shares", func(t *testing.T) {
		itest.SeedSQL(t, db,
			// Create availability zones
			`INSERT INTO availability_zones (id, name, deleted) VALUES
			('az-001', 'nova', 'False'),
			('az-002', 'az2', 'False')`,
			// Create share types
			`INSERT INTO share_types (id, name, deleted) VALUES
			('stype-001', 'default_share_type', 'False'),
			('stype-002', 'premium', 'False')`,
			// Create shares
			`INSERT INTO shares (id, display_name, project_id, size, share_proto, deleted) VALUES
			('share-001', 'my-share', 'proj-001', 100, 'NFS', 'False'),
			('share-002', 'data-share', 'proj-002', 500, 'CIFS', 'False'),
			('share-003', 'deleted-share', 'proj-001', 50, 'NFS', 'True')`,
			// Create share instances (status lives here)
			`INSERT INTO share_instances (id, share_id, status, share_type_id, availability_zone_id, deleted, cast_rules_to_readonly) VALUES
			('si-001', 'share-001', 'available', 'stype-001', 'az-001', 'False', 0),
			('si-002', 'share-002', 'creating', 'stype-002', 'az-002', 'False', 0),
			('si-003', 'share-001', 'available', 'stype-001', 'az-001', 'False', 0),
			('si-004', 'share-001', 'error', 'stype-002', 'az-002', 'False', 0)`,
		)

		collector := NewSharesCollector(db, logger)
		reg := prometheus.NewPedanticRegistry()
		reg.MustRegister(collector)
		response := httptest.NewRecorder()
		promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		families, err := reg.Gather()
		require.NoError(t, err)
		var capacity float64
		for _, family := range families {
			if family.GetName() == "openstack_sharev2_share_bytes" {
				require.Len(t, family.Metric, 2)
				for _, metric := range family.Metric {
					capacity += metric.GetGauge().GetValue()
				}
			}
		}
		require.Equal(t, float64(600*(1<<30)), capacity)

		// 2 active shares × 2 (share_bytes + share_status) = 4
		// + 1 up + 1 shares_counter + 19 status_counter = 25
		count := testutil.CollectAndCount(collector)
		if count != 25 {
			t.Fatalf("expected 25 metrics, got %d", count)
		}

		expected := `# HELP openstack_sharev2_shares_counter shares_counter
# TYPE openstack_sharev2_shares_counter gauge
openstack_sharev2_shares_counter 2
`
		err = testutil.CollectAndCompare(collector, strings.NewReader(expected),
			"openstack_sharev2_shares_counter",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestIntegration_QuotasCollector(t *testing.T) {
	itest.SkipIfNoDocker(t)
	db := itest.NewMySQLContainer(t, "manila", "../../../sql/manila/prereqs.sql", "../../../sql/manila/schema.sql")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	itest.SeedSQL(t, db,
		`INSERT INTO share_types (id, name, deleted) VALUES ('t1', 'gold', 'False'), ('t2', 'silver', 'False'), ('retired', 'retired', 'retired')`,
		`INSERT INTO quota_classes (class_name, resource, hard_limit, deleted) VALUES ('default', 'shares', 80, 0)`,
		`INSERT INTO quotas (project_id, resource, hard_limit, deleted) VALUES ('p1', 'shares', 20, 0), ('p1', 'per_share_gigabytes', 64, 0)`,
		`INSERT INTO project_user_quotas (project_id, user_id, resource, hard_limit, deleted) VALUES ('p1', 'u1', 'shares', 1, 0)`,
		`INSERT INTO project_share_type_quotas (project_id, share_type_id, resource, hard_limit, deleted) VALUES ('p1', 't1', 'shares', 10, 0), ('type-only', 't1', 'shares', -1, 0)`,
		`INSERT INTO quota_usages (project_id, user_id, share_type_id, resource, in_use, reserved, deleted) VALUES
        ('p1', 'u1', NULL, 'shares', 2, 1, 0), ('p1', 'u2', NULL, 'shares', 3, 2, 0),
        ('p1', NULL, 't1', 'shares', 5, 3, 0), ('p1', NULL, 't2', 'shares', 5, 3, 0), ('p1', NULL, 'retired', 'shares', 999, 999, 0), ('p1', NULL, 't1', 'shares', 999, 999, -1), ('p1', 'u1', NULL, 'shares', 999, 999, -1),
        ('usage-only', 'u1', NULL, 'shares', 4, 1, 0)`,
	)
	// Match Manila's integer soft-delete convention for accounting and limits.
	itest.SeedSQL(t, db,
		`UPDATE quota_usages SET deleted = id WHERE deleted = -1`,
		`INSERT INTO quotas (project_id, resource, hard_limit) VALUES ('p1', 'shares', 999)`,
		`UPDATE quotas SET deleted = id WHERE hard_limit = 999`,
		`INSERT INTO quota_classes (class_name, resource, hard_limit) VALUES ('default', 'shares', 999)`,
		`UPDATE quota_classes SET deleted = id WHERE hard_limit = 999`,
		`INSERT INTO project_user_quotas (project_id, user_id, resource, hard_limit) VALUES ('deleted-only', 'u1', 'shares', 999)`,
		`UPDATE project_user_quotas SET deleted = id WHERE hard_limit = 999`,
		`INSERT INTO project_share_type_quotas (project_id, share_type_id, resource, hard_limit) VALUES ('p1', 't1', 'shares', 999)`,
		`UPDATE project_share_type_quotas SET deleted = id WHERE hard_limit = 999`,
	)
	resolver := project.NewResolver(logger, nil, 0)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(NewQuotasCollector(db, logger, resolver, nil), NewSharesCollector(db, logger))
	scrape := func() string {
		t.Helper()
		response := httptest.NewRecorder()
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Equal(t, http.StatusOK, response.Code)
		return response.Body.String()
	}
	body := scrape()
	for _, line := range []string{
		`openstack_sharev2_quota_shares{domain_id="",tenant="p1",tenant_id="p1",type="in_use"} 5`,
		`openstack_sharev2_quota_shares{domain_id="",tenant="p1",tenant_id="p1",type="reserved"} 3`,
		`openstack_sharev2_quota_shares{domain_id="",tenant="p1",tenant_id="p1",type="limit"} 20`,
		`openstack_sharev2_share_type_quota_shares{domain_id="",share_type="t1",share_type_name="gold",tenant="p1",tenant_id="p1",type="in_use"} 5`,
		`openstack_sharev2_share_type_quota_shares{domain_id="",share_type="t1",share_type_name="gold",tenant="p1",tenant_id="p1",type="limit"} 10`,
		`openstack_sharev2_quota_shares{domain_id="",tenant="type-only",tenant_id="type-only",type="limit"} 80`,
		`openstack_sharev2_quota_shares{domain_id="",tenant="usage-only",tenant_id="usage-only",type="in_use"} 4`,
		`openstack_sharev2_share_type_quota_shares{domain_id="",share_type="t1",share_type_name="gold",tenant="p1",tenant_id="p1",type="reserved"} 3`,
		`openstack_sharev2_share_type_quota_shares{domain_id="",share_type="t2",share_type_name="silver",tenant="p1",tenant_id="p1",type="in_use"} 5`,
		`openstack_sharev2_share_type_quota_shares{domain_id="",share_type="t2",share_type_name="silver",tenant="p1",tenant_id="p1",type="limit"} 20`,
		`openstack_sharev2_quota_up 1`, `openstack_sharev2_up 1`,
	} {
		require.Contains(t, body, line+"\n")
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	require.NoError(t, err)
	var perShareLimits []float64
	for _, metric := range families["openstack_sharev2_quota_per_share_bytes"].GetMetric() {
		labels := map[string]string{}
		for _, label := range metric.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["tenant_id"] == "p1" {
			require.Equal(t, map[string]string{"domain_id": "", "tenant": "p1", "tenant_id": "p1", "type": "limit"}, labels)
			require.NotNil(t, metric.Gauge)
			perShareLimits = append(perShareLimits, metric.GetGauge().GetValue())
		}
	}
	require.Equal(t, []float64{64 * (1 << 30)}, perShareLimits)
	require.NotContains(t, body, `tenant_id="deleted-only"`)
	require.NotContains(t, body, `share_type="retired"`)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "openstack_sharev2_quota_per_share_bytes{") {
			require.Contains(t, line, `type="limit"`)
		}
	}
	// A broken quota table must leave real inventory metrics available over HTTP.
	itest.SeedSQL(t, db, "DROP TABLE quota_usages")
	body = scrape()
	require.Contains(t, body, "openstack_sharev2_quota_up 0\n")
	require.Contains(t, body, "openstack_sharev2_up 1\n")
	require.Contains(t, body, "openstack_sharev2_shares_counter 0\n")
	require.NotContains(t, body, "openstack_sharev2_quota_shares")
}
