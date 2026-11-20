package manila

import (
	"bytes"
	"encoding/json"
	dto "github.com/prometheus/client_model/go"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/heat"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	heatdb "github.com/vexxhost/openstackdb/heat/db"
	"io"
	"log/slog"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
	maniladb "github.com/vexxhost/openstackdb/manila/db"
)

// Execute the real generated SELECT against a disposable SQL fixture, then
// feed those actual rows through the real collector, registry and HTTP handler.
func inventoryRows(t *testing.T, query, instances string) *sqlmock.Rows {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("portable SQL fixture requires python3")
	}
	script := `import json, sqlite3, sys
query, instances = json.load(sys.stdin)
db = sqlite3.connect(":memory:")
db.executescript("""
CREATE TABLE shares(id TEXT PRIMARY KEY, display_name TEXT, project_id TEXT, size INT, share_proto TEXT, deleted TEXT, created_at TEXT);
CREATE TABLE share_instances(id TEXT PRIMARY KEY, share_id TEXT, status TEXT, replica_state TEXT, share_type_id TEXT, availability_zone_id TEXT, deleted TEXT);
CREATE TABLE share_types(id TEXT PRIMARY KEY, name TEXT, deleted TEXT);
CREATE TABLE availability_zones(id TEXT PRIMARY KEY, name TEXT, deleted TEXT);
INSERT INTO shares VALUES('share-1','share','project-1',10,'NFS','False',NULL);
INSERT INTO share_types VALUES('type-1','gold','False'),('type-2','silver','False');
INSERT INTO availability_zones VALUES('az-1','zone-1','False'),('az-2','zone-2','False');
""")
db.executescript(instances)
print(json.dumps(db.execute(query).fetchall()))
`
	input, err := json.Marshal([]string{query, instances})
	require.NoError(t, err)
	cmd := exec.Command(python, "-c", script)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	var values [][]interface{}
	require.NoError(t, json.Unmarshal(output, &values))
	rows := sqlmock.NewRows([]string{"id", "name", "project_id", "size", "share_proto", "status", "share_type", "share_type_name", "availability_zone"})
	for _, row := range values {
		rows.AddRow(row[0], row[1], row[2], row[3], row[4], row[5], row[6], row[7], row[8])
	}
	return rows
}

// Each expected row is status, share type, and availability zone, by logical ID.
func TestSharesInventoryHTTP(t *testing.T) {
	const shareCapacityBytes = float64(10 * (1 << 30))
	one := func(status, typ, az string) map[string][3]string {
		return map[string][3]string{"share-1": {status, typ, az}}
	}
	for _, tc := range []struct {
		name, instances string
		want            map[string][3]string
	}{
		{"zero", "", one("", "", "")},
		{"one", "INSERT INTO share_instances VALUES('i1','share-1','available',NULL,'type-1','az-1','False');", one("available", "type-1", "zone-1")},
		{"identical", "INSERT INTO share_instances VALUES('i1','share-1','available',NULL,'type-1','az-1','False'),('i2','share-1','available',NULL,'type-1','az-1','False');", one("available", "type-1", "zone-1")},
		{"different", "INSERT INTO share_instances VALUES('i1','share-1','available','active','type-1','az-1','False'),('i2','share-1','error','in_sync','type-2','az-2','False');", one("available", "type-1", "zone-1")},
		{"same-status-different-AZ", "INSERT INTO share_instances VALUES('i1','share-1','available',NULL,'type-1','az-1','False'),('i2','share-1','available',NULL,'type-1','az-2','False');", one("available", "type-1", "zone-1")},
		{"replicated-active-error", "INSERT INTO share_instances VALUES('i1','share-1','available','in_sync','type-1','az-1','False'),('i2','share-1','error','active','type-2','az-2','False');", one("error", "type-2", "zone-2")},
		{"promotion", "INSERT INTO share_instances VALUES('i1','share-1','available','active','type-1','az-1','False'),('i2','share-1','replication_change','in_sync','type-2','az-2','False');", one("replication_change", "type-2", "zone-2")},
		{"reverting-promotion-exception", "INSERT INTO share_instances VALUES('i1','share-1','available','active','type-1','az-1','False'),('i2','share-1','replication_change','in_sync','type-2','az-2','False'),('i3','share-1','reverting','in_sync','type-2','az-2','False');", one("available", "type-1", "zone-1")},
		{"writable-multiple-active", "INSERT INTO share_instances VALUES('i2','share-1','available','active','type-2','az-2','False'),('i1','share-1','available','active','type-1','az-1','False');", one("available", "type-1", "zone-1")},
		{"migration", "INSERT INTO share_instances VALUES('i1','share-1','migrating_to',NULL,'type-1','az-1','False'),('i2','share-1','migrating',NULL,'type-2','az-2','False');", one("migrating", "type-2", "zone-2")},
		{"snapshot-revert-priority", "INSERT INTO share_instances VALUES('i1','share-1','reverting_to_snapshot',NULL,'type-1','az-1','False'),('i2','share-1','creating',NULL,'type-2','az-2','False');", one("creating", "type-2", "zone-2")},
		{"server-migration", "INSERT INTO share_instances VALUES('i1','share-1','server_migrating_to',NULL,'type-1','az-1','False'),('i2','share-1','server_migrating',NULL,'type-2','az-2','False');", one("server_migrating", "type-2", "zone-2")},
		{"soft-deleted-instance", "INSERT INTO share_instances VALUES('i1','share-1','reverting','active','type-1','az-1','removed'),('i2','share-1','available',NULL,'type-2','az-2','False');", one("available", "type-2", "zone-2")},
		{"only-deleted-instance", "INSERT INTO share_instances VALUES('i1','share-1','available',NULL,'type-1','az-1','removed');", one("", "", "")},
		{"multiple-projects", "INSERT INTO shares VALUES('share-2','other','project-2',10,'NFS','False',NULL),('removed','gone','project-2',100,'NFS','removed',NULL); INSERT INTO share_instances VALUES('i1','share-1','available',NULL,'type-1','az-1','False'),('i2','share-1','available',NULL,'type-1','az-1','False'),('i3','share-2','error',NULL,'type-2','az-2','False');", map[string][3]string{"share-1": {"available", "type-1", "zone-1"}, "share-2": {"error", "type-2", "zone-2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.MatchExpectationsInOrder(false)
			for range 3 {
				mock.ExpectQuery(regexp.QuoteMeta(maniladb.ShareGetInventory)).WillReturnRows(inventoryRows(t, maniladb.ShareGetInventory, tc.instances))
			}
			for range 2 {
				rows := emptyQuotaRows()
				rows[3].AddRow("project-1", nil, "shares", 1, 0)
				expectQuotaRows(mock, rows)
				mock.ExpectQuery(regexp.QuoteMeta(heatdb.StackGetAll)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "action", "tenant"}).AddRow("stack-1", "stack", "CREATE_COMPLETE", "CREATE", "project-1"))
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			collector := NewSharesCollector(db, logger)
			// Measure raw emissions too: Gather discards colliding samples on error.
			raw := make(chan prometheus.Metric, 100)
			collector.Collect(raw)
			close(raw)
			var rawCount, rawCapacity float64
			for m := range raw {
				metric := &dto.Metric{}
				require.NoError(t, m.Write(metric))
				if strings.Contains(m.Desc().String(), `fqName: "openstack_sharev2_shares_counter"`) {
					rawCount += metric.GetGauge().GetValue()
				}
				if strings.Contains(m.Desc().String(), `fqName: "openstack_sharev2_share_bytes"`) {
					rawCapacity += metric.GetGauge().GetValue()
				}
			}
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(collector, NewQuotasCollector(db, logger, project.NewResolver(logger, nil, 0), nil), heat.NewStacksCollector(db, logger))
			families, gatherErr := registry.Gather()
			var count, capacity float64
			statusCounts := map[string]float64{}
			for _, metadata := range tc.want {
				statusCounts[metadata[0]]++
			}
			capacityIDs, statusIDs := map[string]bool{}, map[string]bool{}
			familyNames := map[string]bool{}
			for _, f := range families {
				familyNames[f.GetName()] = true
				seen := map[string]bool{}
				for _, m := range f.Metric {
					labels := map[string]string{}
					key := ""
					for _, label := range m.Label {
						labels[label.GetName()] = label.GetValue()
						key += label.String()
					}
					require.False(t, seen[key], "duplicate labels in %s", f.GetName())
					seen[key] = true
					switch f.GetName() {
					case "openstack_sharev2_shares_counter":
						count += m.GetGauge().GetValue()
					case "openstack_sharev2_share_status_counter":
						require.Equal(t, statusCounts[labels["status"]], m.GetGauge().GetValue())
					case "openstack_sharev2_share_bytes", "openstack_sharev2_share_status":
						expected, ok := tc.want[labels["id"]]
						require.True(t, ok)
						require.Equal(t, expected[0], labels["status"])
						require.Equal(t, expected[1], labels["share_type"])
						if f.GetName() == "openstack_sharev2_share_bytes" {
							require.False(t, capacityIDs[labels["id"]])
							capacityIDs[labels["id"]] = true
							require.Equal(t, expected[2], labels["availability_zone"])
							require.Equal(t, shareCapacityBytes, m.GetGauge().GetValue())
							capacity += m.GetGauge().GetValue()
						} else {
							require.False(t, statusIDs[labels["id"]])
							statusIDs[labels["id"]] = true
						}
					case "openstack_sharev2_quota_shares":
						if labels["type"] == "in_use" {
							require.Equal(t, float64(1), m.GetGauge().GetValue())
						}
					}
				}
			}
			response := httptest.NewRecorder()
			promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
			t.Logf("raw_count=%g raw_capacity=%g gathered_count=%g gathered_capacity=%g gather_error=%v HTTP=%d", rawCount, rawCapacity, count, capacity, gatherErr, response.Code)
			require.NoError(t, gatherErr)
			require.True(t, familyNames["openstack_sharev2_share_bytes"])
			require.False(t, familyNames["openstack_sharev2_share_gb"])
			require.Equal(t, 200, response.Code)
			require.Contains(t, response.Body.String(), "\nopenstack_sharev2_share_bytes{")
			require.NotContains(t, response.Body.String(), "openstack_sharev2_share_gb")
			require.Equal(t, float64(len(tc.want)), rawCount)
			require.Equal(t, shareCapacityBytes*float64(len(tc.want)), rawCapacity)
			require.Equal(t, float64(len(tc.want)), count)
			require.Equal(t, shareCapacityBytes*float64(len(tc.want)), capacity)
			require.Len(t, capacityIDs, len(tc.want))
			require.Len(t, statusIDs, len(tc.want))
			require.Contains(t, response.Body.String(), "openstack_heat_up 1")
			require.Contains(t, response.Body.String(), `openstack_heat_stack_status_counter{status="CREATE_COMPLETE"} 1`)
			require.Contains(t, response.Body.String(), "openstack_sharev2_quota_up 1")
			require.Contains(t, response.Body.String(), "openstack_sharev2_quota_shares{")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
