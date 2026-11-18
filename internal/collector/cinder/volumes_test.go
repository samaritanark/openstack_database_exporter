package cinder

import (
	"bytes"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/heat"
	"github.com/vexxhost/openstack_database_exporter/internal/testutil"
	cinderdb "github.com/vexxhost/openstackdb/cinder/db"
	heatdb "github.com/vexxhost/openstackdb/heat/db"
)

func TestVolumesCollector(t *testing.T) {
	tests := []testutil.CollectorTestCase{
		{
			Name: "successful collection with volumes",
			SetupMock: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{
					"id", "name", "size", "status", "availability_zone",
					"bootable", "project_id", "user_id", "volume_type", "server_id",
				}).AddRow(
					"173f7b48-c4c1-4e70-9acc-086b39073506", "test-volume", 1, "available", "nova",
					true, "bab7d5c60cd041a0a36f7c4b6e1dd978", "32779452fcd34ae1a53a797ac8a1e064", "lvmdriver-1", nil,
				).AddRow(
					"6edbc2f4-1507-44f8-ac0d-eed1d2608d38", "test-volume-attachments", 2, "in-use", "nova",
					false, "bab7d5c60cd041a0a36f7c4b6e1dd978", "32779452fcd34ae1a53a797ac8a1e064", "lvmdriver-1", "f4fda93b-06e0-4743-8117-bc8bcecd651b",
				)
				mock.ExpectQuery(regexp.QuoteMeta(cinderdb.VolumeGetAllWithAttachments)).WillReturnRows(rows)
			},
			ExpectedMetrics: `# HELP openstack_cinder_up up
# TYPE openstack_cinder_up gauge
openstack_cinder_up 1
# HELP openstack_cinder_volume_gb volume_gb
# TYPE openstack_cinder_volume_gb gauge
openstack_cinder_volume_gb{availability_zone="nova",bootable="false",id="6edbc2f4-1507-44f8-ac0d-eed1d2608d38",name="test-volume-attachments",server_id="f4fda93b-06e0-4743-8117-bc8bcecd651b",status="in-use",tenant_id="bab7d5c60cd041a0a36f7c4b6e1dd978",user_id="32779452fcd34ae1a53a797ac8a1e064",volume_type="lvmdriver-1"} 2
openstack_cinder_volume_gb{availability_zone="nova",bootable="true",id="173f7b48-c4c1-4e70-9acc-086b39073506",name="test-volume",server_id="",status="available",tenant_id="bab7d5c60cd041a0a36f7c4b6e1dd978",user_id="32779452fcd34ae1a53a797ac8a1e064",volume_type="lvmdriver-1"} 1
# HELP openstack_cinder_volume_status volume_status
# TYPE openstack_cinder_volume_status gauge
openstack_cinder_volume_status{bootable="false",id="6edbc2f4-1507-44f8-ac0d-eed1d2608d38",name="test-volume-attachments",server_id="f4fda93b-06e0-4743-8117-bc8bcecd651b",size="2",status="in-use",tenant_id="bab7d5c60cd041a0a36f7c4b6e1dd978",volume_type="lvmdriver-1"} 5
openstack_cinder_volume_status{bootable="true",id="173f7b48-c4c1-4e70-9acc-086b39073506",name="test-volume",server_id="",size="1",status="available",tenant_id="bab7d5c60cd041a0a36f7c4b6e1dd978",volume_type="lvmdriver-1"} 1
# HELP openstack_cinder_volume_status_counter volume_status_counter
# TYPE openstack_cinder_volume_status_counter gauge
openstack_cinder_volume_status_counter{status="attaching"} 0
openstack_cinder_volume_status_counter{status="available"} 1
openstack_cinder_volume_status_counter{status="awaiting-transfer"} 0
openstack_cinder_volume_status_counter{status="backing-up"} 0
openstack_cinder_volume_status_counter{status="creating"} 0
openstack_cinder_volume_status_counter{status="deleting"} 0
openstack_cinder_volume_status_counter{status="detaching"} 0
openstack_cinder_volume_status_counter{status="downloading"} 0
openstack_cinder_volume_status_counter{status="error"} 0
openstack_cinder_volume_status_counter{status="error_backing-up"} 0
openstack_cinder_volume_status_counter{status="error_deleting"} 0
openstack_cinder_volume_status_counter{status="error_extending"} 0
openstack_cinder_volume_status_counter{status="error_restoring"} 0
openstack_cinder_volume_status_counter{status="extending"} 0
openstack_cinder_volume_status_counter{status="in-use"} 1
openstack_cinder_volume_status_counter{status="maintenance"} 0
openstack_cinder_volume_status_counter{status="reserved"} 0
openstack_cinder_volume_status_counter{status="restoring-backup"} 0
openstack_cinder_volume_status_counter{status="retyping"} 0
openstack_cinder_volume_status_counter{status="uploading"} 0
# HELP openstack_cinder_volumes volumes
# TYPE openstack_cinder_volumes gauge
openstack_cinder_volumes 2
`,
		},
		{
			Name: "query error",
			SetupMock: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(regexp.QuoteMeta(cinderdb.VolumeGetAllWithAttachments)).WillReturnError(sql.ErrConnDone)
			},
			ExpectedMetrics: "",
			ExpectError:     false,
		},
	}

	testutil.RunCollectorTests(t, tests, NewVolumesCollector)
}

func TestVolumesCollectorAttachmentRows(t *testing.T) {
	tests := []struct {
		name    string
		servers []any
		want    string
	}{
		{name: "unattached", servers: []any{nil}, want: ""},
		{name: "one attachment", servers: []any{"server-a"}, want: "server-a"},
		// Soft-deleted history is excluded by the SQL join. Nondeleted historical
		// rows cannot be distinguished from current attachments in this projection;
		// retain their server IDs rather than choosing an arbitrary attachment.
		{name: "nondeleted historical attachments", servers: []any{"old-server", "server-a", "old-server"}, want: "old-server,server-a"},
		{name: "multiple active attachments", servers: []any{"server-b", "server-a"}, want: "server-a,server-b"},
		{name: "four identical join rows", servers: []any{"server-a", "server-a", "server-a", "server-a"}, want: "server-a"},
		{name: "duplicate multiattachment rows", servers: []any{"server-b", "server-a", "server-b", "server-a"}, want: "server-a,server-b"},
		{name: "unknown servers alongside known servers", servers: []any{nil, "", "server-b", "server-a", nil}, want: "server-a,server-b"},
		{name: "duplicate unknown servers", servers: []any{nil, "", nil, ""}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var baseline string
			// Rotate and reverse the rows, also changing where a different volume
			// appears, to catch first/last-row selection.
			for order := 0; order < 2*len(tt.servers); order++ {
				servers := slices.Clone(tt.servers)
				if order%2 != 0 {
					slices.Reverse(servers)
				}
				offset := order / 2
				servers = append(servers[offset:], servers[:offset]...)
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				// Both collectors share one mock connection and one close expectation.
				db.SetMaxOpenConns(1)
				t.Cleanup(func() {
					mock.ExpectClose()
					if err := db.Close(); err != nil {
						t.Errorf("close db: %v", err)
					}
				})
				mock.MatchExpectationsInOrder(false)
				// Supply the same fixture for a pedantic gather and two HTTP scrapes.
				for scrape := 0; scrape < 3; scrape++ {
					rows := sqlmock.NewRows([]string{
						"id", "name", "size", "status", "availability_zone",
						"bootable", "project_id", "user_id", "volume_type", "server_id",
					})
					for i, server := range servers {
						rows.AddRow("volume", "test", 7, "in-use", "nova", true, "project", "user", "fast", server)
						if i == order%len(servers) {
							rows.AddRow("unattached", "other", 3, "available", "nova", false, "project", "user", "fast", nil)
						}
					}
					mock.ExpectQuery(regexp.QuoteMeta(cinderdb.VolumeGetAllWithAttachments)).WillReturnRows(rows)
					mock.ExpectQuery(regexp.QuoteMeta(heatdb.StackGetAll)).WillReturnRows(
						sqlmock.NewRows([]string{"id", "name", "status", "action", "tenant"}).
							AddRow("stack", "test", "CREATE_COMPLETE", "CREATE", "project"))
				}
				registry := prometheus.NewPedanticRegistry()
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				registry.MustRegister(NewVolumesCollector(db, logger), heat.NewStacksCollector(db, logger))
				families, err := registry.Gather()
				require.NoError(t, err, "duplicate series must not break gathering")
				require.Len(t, families, 7)
				var output bytes.Buffer
				for _, family := range families {
					require.Equal(t, dto.MetricType_GAUGE, family.GetType())
					_, err := expfmt.MetricFamilyToText(&output, family)
					require.NoError(t, err)
					switch family.GetName() {
					case "openstack_cinder_volume_gb", "openstack_cinder_volume_status":
						require.Len(t, family.Metric, 2)
						var total float64
						for _, metric := range family.Metric {
							labels := map[string]string{}
							for _, label := range metric.Label {
								labels[label.GetName()] = label.GetValue()
							}
							if labels["id"] == "volume" {
								require.Equal(t, tt.want, labels["server_id"])
								if family.GetName() == "openstack_cinder_volume_status" {
									require.Equal(t, "7", labels["size"], "size label must remain in GiB")
									require.Equal(t, float64(5), metric.GetGauge().GetValue())
								} else {
									require.Equal(t, float64(7), metric.GetGauge().GetValue(), "capacity must remain in GiB")
								}
							} else {
								require.Equal(t, "unattached", labels["id"])
								require.Empty(t, labels["server_id"])
								if family.GetName() == "openstack_cinder_volume_status" {
									require.Equal(t, "3", labels["size"])
									require.Equal(t, float64(1), metric.GetGauge().GetValue())
								} else {
									require.Equal(t, float64(3), metric.GetGauge().GetValue())
								}
							}
							total += metric.GetGauge().GetValue()
						}
						if family.GetName() == "openstack_cinder_volume_gb" {
							require.Equal(t, float64(10), total, "capacity must count each volume once")
						}
					case "openstack_cinder_volumes":
						require.Equal(t, float64(2), family.Metric[0].GetGauge().GetValue())
					case "openstack_cinder_volume_status_counter":
						var total float64
						for _, metric := range family.Metric {
							want := float64(0)
							status := metric.Label[0].GetValue()
							if status == "in-use" || status == "available" {
								want = 1
							}
							require.Equal(t, want, metric.GetGauge().GetValue())
							total += metric.GetGauge().GetValue()
						}
						require.Equal(t, float64(2), total)
					case "openstack_cinder_up", "openstack_heat_up":
						require.Equal(t, float64(1), family.Metric[0].GetGauge().GetValue())
					case "openstack_heat_stack_status_counter":
						for _, metric := range family.Metric {
							want := float64(0)
							if metric.Label[0].GetValue() == "CREATE_COMPLETE" {
								want = 1
							}
							require.Equal(t, want, metric.GetGauge().GetValue())
						}
					}
				}
				mux := http.NewServeMux()
				mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
				for scrape := 0; scrape < 2; scrape++ {
					request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
					request.Header.Set("Accept", "text/plain; version=0.0.4")
					response := httptest.NewRecorder()
					mux.ServeHTTP(response, request)
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					require.Equal(t, output.String(), response.Body.String(), "HTTP must preserve all gathered metrics across scrapes")
				}
				require.NoError(t, mock.ExpectationsWereMet())
				if order == 0 {
					baseline = output.String()
				} else {
					require.Equal(t, baseline, output.String(), "row order must not change exposition")
				}
			}
		})
	}
}
