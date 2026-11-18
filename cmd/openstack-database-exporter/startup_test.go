package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector"
)

func TestOctaviaQuotaStartup(t *testing.T) {
	valid := `{"octavia":{"loadbalancer":0,"listener":1,"pool":2,"member":-1,"healthmonitor":3,"l7policy":4,"l7rule":5},"nova":{"unrelated":"value"}}`
	for _, enabled := range []bool{false, true} {
		for name, content := range map[string]string{"absent path": "", "missing file": "missing", "malformed": "{", "incomplete": `{"octavia":{"member":1}}`, "unrelated only": `{"nova":{}}`, "valid": valid} {
			t.Run(name+map[bool]string{false: " disabled", true: " enabled"}[enabled], func(t *testing.T) {
				path := ""
				if name != "absent path" {
					path = filepath.Join(t.TempDir(), "defaults.json")
					if name != "missing file" {
						require.NoError(t, os.WriteFile(path, []byte(content), 0600))
					}
				}
				url := ""
				if enabled {
					url = "unsupported://octavia/test"
				}
				defaults, err := loadOctaviaQuotaConfig(url, path, 5*time.Second)
				if enabled && name != "valid" && name != "absent path" {
					require.ErrorContains(t, err, "octavia quota defaults file")
					return
				}
				require.NoError(t, err)
				if enabled {
					require.NoError(t, defaults.Validate())
					if name == "absent path" {
						for _, value := range defaults {
							require.EqualValues(t, -1, value)
						}
					} else {
						require.EqualValues(t, 0, defaults["loadbalancer"])
						require.EqualValues(t, -1, defaults["member"])
					}
				} else {
					require.Nil(t, defaults)
				}
				// Unsupported synthetic URLs cannot connect to any database. The Nova
				// down gauge proves its collector was registered despite irrelevant files.
				reg := collector.NewRegistry(collector.Config{NovaDatabaseURL: "unsupported://nova/test", NovaAPIDatabaseURL: "unsupported://nova-api/test", OctaviaDatabaseURL: url, OctaviaQuotaDefaults: defaults}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				response := httptest.NewRecorder()
				promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				require.Equal(t, http.StatusOK, response.Code)
				require.Contains(t, response.Body.String(), "openstack_nova_up 0\n")
				if !enabled {
					require.NotContains(t, response.Body.String(), "openstack_loadbalancer_quota_up")
				}
			})
		}
	}
}

func TestOctaviaQuotaStartupTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		_, err := loadOctaviaQuotaConfig("unsupported://octavia/test", "", timeout)
		require.ErrorContains(t, err, "timeout must be positive")
		_, err = loadOctaviaQuotaConfig("", "irrelevant", timeout)
		require.NoError(t, err)
	}
}
