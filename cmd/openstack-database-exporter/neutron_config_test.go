package main

import (
	"bytes"
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

func TestNeutronQuotaConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disabled := loadNeutronQuotaConfig("", "missing", -time.Second, logger)
	require.NoError(t, disabled.ConfigError)
	for _, timeout := range []time.Duration{0, -time.Second} {
		opts := loadNeutronQuotaConfig("configured", "", timeout, logger)
		require.Error(t, opts.ConfigError)
	}
	opts := loadNeutronQuotaConfig("configured", "missing", time.Second, logger)
	require.Error(t, opts.ConfigError)
	opts = loadNeutronQuotaConfig("configured", "", time.Second, logger)
	require.NoError(t, opts.ConfigError)
	require.NoError(t, opts.Defaults.Validate())
}

func TestNeutronInvalidConfigurationKeepsExporterOperational(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Second, time.Second} {
		var logs bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		path := filepath.Join(t.TempDir(), "defaults.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"neutron":{"port":"mysql://secret:password@host"}}`), 0600))
		opts := loadNeutronQuotaConfig("configured", path, timeout, logger)
		require.Error(t, opts.ConfigError)
		reg := collector.NewRegistry(collector.Config{
			NeutronDatabaseURL: "unsupported://neutron/test", NeutronQuotaOptions: opts,
			NovaDatabaseURL: "unsupported://nova/test", NovaAPIDatabaseURL: "unsupported://nova-api/test",
		}, logger)
		before := logs.String()
		for range 2 {
			response := httptest.NewRecorder()
			promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			require.Equal(t, http.StatusOK, response.Code)
			require.Contains(t, response.Body.String(), "openstack_neutron_quota_up 0\n")
			require.Contains(t, response.Body.String(), "openstack_nova_up 0\n")
		}
		require.Equal(t, before, logs.String())
		require.Contains(t, logs.String(), "Invalid Neutron quota configuration")
		for _, secret := range []string{"secret", "password", "mysql://"} {
			require.NotContains(t, logs.String(), secret)
		}
	}
}
