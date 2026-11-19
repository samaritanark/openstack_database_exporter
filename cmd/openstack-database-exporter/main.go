package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/vexxhost/openstack_database_exporter/internal/collector"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/cinder"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/octavia"
)

var (
	cinderQuotaQueryTimeout = kingpin.Flag("cinder.quota-query-timeout", "Shared deadline for Cinder quota SQL reads; must be positive and shorter than the scrape timeout.").Default("5s").Envar("CINDER_QUOTA_QUERY_TIMEOUT").Duration()
	quotaQueryTimeout       = kingpin.Flag("octavia.quota-query-timeout", "Shared deadline for Octavia quota database queries; must be positive and shorter than the scrape timeout.").Default("5s").Envar("OCTAVIA_QUOTA_QUERY_TIMEOUT").Duration()
	quotaDefaultsFile       = kingpin.Flag("quota.defaults-file", "Optional JSON file containing complete quota defaults for enabled Octavia and Cinder services; otherwise each service's upstream 2025.2 defaults are assumed.").Envar("QUOTA_DEFAULTS_FILE").String()

	metricsPath = kingpin.Flag(
		"web.telemetry-path",
		"Path under which to expose metrics.",
	).Default("/metrics").String()
	toolkitFlags = webflag.AddFlags(kingpin.CommandLine, ":9180")

	// Database connection flags
	cinderDatabaseURL = kingpin.Flag(
		"cinder.database-url",
		"Cinder database connection URL (oslo.db format)",
	).Envar("CINDER_DATABASE_URL").String()
	glanceDatabaseURL = kingpin.Flag(
		"glance.database-url",
		"Glance database connection URL (oslo.db format)",
	).Envar("GLANCE_DATABASE_URL").String()
	heatDatabaseURL = kingpin.Flag(
		"heat.database-url",
		"Heat database connection URL (oslo.db format)",
	).Envar("HEAT_DATABASE_URL").String()
	ironicDatabaseURL = kingpin.Flag(
		"ironic.database-url",
		"Ironic database connection URL (oslo.db format)",
	).Envar("IRONIC_DATABASE_URL").String()
	keystoneDatabaseURL = kingpin.Flag(
		"keystone.database-url",
		"Keystone database connection URL (oslo.db format)",
	).Envar("KEYSTONE_DATABASE_URL").String()
	magnumDatabaseURL = kingpin.Flag(
		"magnum.database-url",
		"Magnum database connection URL (oslo.db format)",
	).Envar("MAGNUM_DATABASE_URL").String()
	manilaDatabaseURL = kingpin.Flag(
		"manila.database-url",
		"Manila database connection URL (oslo.db format)",
	).Envar("MANILA_DATABASE_URL").String()
	neutronDatabaseURL = kingpin.Flag(
		"neutron.database-url",
		"Neutron database connection URL (oslo.db format)",
	).Envar("NEUTRON_DATABASE_URL").String()
	octaviaDatabaseURL = kingpin.Flag(
		"octavia.database-url",
		"Octavia database connection URL (oslo.db format)",
	).Envar("OCTAVIA_DATABASE_URL").String()
	placementDatabaseURL = kingpin.Flag(
		"placement.database-url",
		"Placement database connection URL (oslo.db format)",
	).Envar("PLACEMENT_DATABASE_URL").String()
	novaDatabaseURL = kingpin.Flag(
		"nova.database-url",
		"Nova database connection URL (oslo.db format)",
	).Envar("NOVA_DATABASE_URL").String()
	novaAPIDatabaseURL = kingpin.Flag(
		"nova-api.database-url",
		"Nova API database connection URL (oslo.db format)",
	).Envar("NOVA_API_DATABASE_URL").String()
	projectCacheTTL = kingpin.Flag(
		"project-cache-ttl",
		"TTL for the keystone project name cache (default 5m).",
	).Default("5m").Envar("PROJECT_CACHE_TTL").Duration()
)

func main() {
	promslogConfig := &promslog.Config{}
	flag.AddFlags(kingpin.CommandLine, promslogConfig)

	kingpin.Version(version.Print("openstack_database_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := promslog.New(promslogConfig)

	logger.Info("Starting openstack_database_exporter", "version", version.Info())
	logger.Info("Build context", "build_context", version.BuildContext())

	quotaDefaults, err := loadOctaviaQuotaConfig(*octaviaDatabaseURL, *quotaDefaultsFile, *quotaQueryTimeout)
	if err != nil {
		logger.Error("Invalid Octavia quota configuration", "error", err)
		os.Exit(1)
	}
	if *octaviaDatabaseURL != "" && *quotaDefaultsFile == "" {
		logger.Warn("No Octavia quota defaults file configured; assuming Octavia 2025.2 upstream unlimited (-1) defaults")
	}
	cinderDefaults, err := loadCinderQuotaConfig(*cinderDatabaseURL, *quotaDefaultsFile, *cinderQuotaQueryTimeout, logger)
	if err != nil {
		logger.Error("Invalid Cinder quota configuration", "error", err)
		os.Exit(1)
	}
	reg := collector.NewRegistry(collector.Config{
		CinderDatabaseURL:        *cinderDatabaseURL,
		CinderQuotaDefaults:      cinderDefaults,
		CinderQuotaQueryTimeout:  *cinderQuotaQueryTimeout,
		GlanceDatabaseURL:        *glanceDatabaseURL,
		HeatDatabaseURL:          *heatDatabaseURL,
		IronicDatabaseURL:        *ironicDatabaseURL,
		KeystoneDatabaseURL:      *keystoneDatabaseURL,
		MagnumDatabaseURL:        *magnumDatabaseURL,
		ManilaDatabaseURL:        *manilaDatabaseURL,
		NeutronDatabaseURL:       *neutronDatabaseURL,
		OctaviaDatabaseURL:       *octaviaDatabaseURL,
		OctaviaQuotaDefaults:     quotaDefaults,
		OctaviaQuotaQueryTimeout: *quotaQueryTimeout,
		PlacementDatabaseURL:     *placementDatabaseURL,
		NovaDatabaseURL:          *novaDatabaseURL,
		NovaAPIDatabaseURL:       *novaAPIDatabaseURL,
		ProjectCacheTTL:          *projectCacheTTL,
	}, logger)

	http.Handle(*metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	if *metricsPath != "/" && *metricsPath != "" {
		landingPage, err := web.NewLandingPage(web.LandingConfig{
			Name:        "OpenStack Database Exporter",
			Description: "Prometheus Exporter for OpenStack Databases",
			Version:     version.Info(),
			Profiling:   "false",
			Links: []web.LandingLinks{
				{Address: *metricsPath, Text: "Metrics"},
			},
		})
		if err != nil {
			logger.Error("failed to create landing page", "err", err)
			os.Exit(1)
		}
		http.Handle("/", landingPage)
	}

	srv := &http.Server{}
	if err := web.ListenAndServe(srv, toolkitFlags, logger); err != nil {
		logger.Error("Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}

// Disabled Octavia must not read or validate configuration for its collectors.
func loadOctaviaQuotaConfig(databaseURL, defaultsFile string, timeout time.Duration) (octavia.QuotaDefaults, error) {
	if databaseURL == "" {
		return nil, nil
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("octavia quota query timeout must be positive")
	}
	defaults, err := octavia.LoadQuotaDefaults(defaultsFile)
	if err != nil {
		return nil, fmt.Errorf("octavia quota defaults file %q: %w", defaultsFile, err)
	}
	return defaults, nil
}

func loadCinderQuotaConfig(databaseURL, defaultsFile string, timeout time.Duration, logger *slog.Logger) (*cinder.QuotaDefaults, error) {
	if databaseURL == "" {
		return nil, nil
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("cinder quota query timeout must be positive")
	}
	defaults, err := cinder.LoadQuotaDefaults(defaultsFile, logger)
	if err != nil {
		return nil, fmt.Errorf("cinder quota defaults file %q: %w", defaultsFile, err)
	}
	return defaults, nil
}
