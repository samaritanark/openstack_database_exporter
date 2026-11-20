package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
	"github.com/vexxhost/openstack_database_exporter/internal/collector"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/manila"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/octavia"
)

func TestManilaQuotaStartup(t *testing.T) {
	valid, err := manila.LoadQuotaDefaults("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	valid["shares"] = 0
	valid["backups"] = -1
	body, err := json.Marshal(map[string]any{"manila": valid, "nova": map[string]string{"unrelated": "value"}})
	require.NoError(t, err)
	for _, enabled := range []bool{false, true} {
		for name, content := range map[string]string{"absent path": "", "missing file": "missing", "malformed": "{", "incomplete": `{"manila":{"shares":1}}`, "unrelated only": `{"octavia":{}}`, "valid": string(body)} {
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
					url = "unsupported://manila/test"
				}
				defaults, err := loadManilaQuotaConfig(url, path, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if enabled && name != "valid" && name != "absent path" && name != "unrelated only" {
					require.ErrorContains(t, err, "manila quota defaults file")
					return
				}
				require.NoError(t, err)
				if enabled {
					require.NoError(t, defaults.Validate())
					if name == "valid" {
						require.EqualValues(t, 0, defaults["shares"])
						require.EqualValues(t, -1, defaults["backups"])
					} else {
						require.EqualValues(t, 50, defaults["shares"])
					}
				} else {
					require.Nil(t, defaults)
				}
				reg := collector.NewRegistry(collector.Config{ManilaDatabaseURL: url, ManilaQuotaDefaults: defaults, NovaDatabaseURL: "unsupported://nova/test", NovaAPIDatabaseURL: "unsupported://nova-api/test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				response := httptest.NewRecorder()
				promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				require.Equal(t, http.StatusOK, response.Code)
				require.Contains(t, response.Body.String(), "openstack_nova_up 0\n")
				if enabled {
					require.Contains(t, response.Body.String(), "openstack_sharev2_quota_up 0\n")
				} else {
					require.NotContains(t, response.Body.String(), "openstack_sharev2_quota_up")
				}
			})
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		_, err := loadManilaQuotaConfig("unsupported://manila/test", "", timeout, slog.New(slog.NewTextHandler(io.Discard, nil)))
		require.ErrorContains(t, err, "timeout must be positive")
		_, err = loadManilaQuotaConfig("", "irrelevant", timeout, slog.New(slog.NewTextHandler(io.Discard, nil)))
		require.NoError(t, err)
	}
}

func TestSharedQuotaDefaultsFile(t *testing.T) {
	m, err := manila.LoadQuotaDefaults("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	o, err := octavia.LoadQuotaDefaults("")
	require.NoError(t, err)
	negative := maps.Clone(m)
	negative["shares"] = -2
	unknown := maps.Clone(m)
	unknown["typo"] = 1
	for name, section := range map[string]any{
		"absent": nil, "valid": m, "null": json.RawMessage(`null`),
		"malformed": []int{1}, "incomplete": map[string]int{"shares": 1},
		"negative": negative, "unknown": unknown,
	} {
		t.Run(name, func(t *testing.T) {
			services := map[string]any{"octavia": o}
			if name != "absent" {
				services["manila"] = section
			}
			body, err := json.Marshal(services)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "defaults.json")
			require.NoError(t, os.WriteFile(path, body, 0600))
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			gotO, err := loadOctaviaQuotaConfig("enabled", path, time.Second)
			require.NoError(t, err)
			require.Equal(t, o, gotO)
			gotM, err := loadManilaQuotaConfig("enabled", path, time.Second, logger)
			if name != "absent" && name != "valid" {
				require.ErrorContains(t, err, "manila quota")
				require.Empty(t, logs.String())
				return
			}
			require.NoError(t, err)
			require.Equal(t, m, gotM)
			if name == "absent" {
				require.Contains(t, logs.String(), "level=WARN")
				require.Contains(t, logs.String(), "No Manila section")
				require.Contains(t, logs.String(), "customized manila.conf")
			}
			reg := collector.NewRegistry(collector.Config{
				ManilaDatabaseURL: "unsupported://manila/test", ManilaQuotaDefaults: gotM,
				OctaviaDatabaseURL: "unsupported://octavia/test", OctaviaQuotaDefaults: gotO,
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			response := httptest.NewRecorder()
			promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			require.Equal(t, http.StatusOK, response.Code)
			require.Contains(t, response.Body.String(), "openstack_sharev2_quota_up 0\n")
			require.Contains(t, response.Body.String(), "openstack_loadbalancer_quota_up 0\n")
		})
	}
}
