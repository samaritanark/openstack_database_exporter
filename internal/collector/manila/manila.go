package manila

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	"github.com/vexxhost/openstack_database_exporter/internal/util"
	db "github.com/vexxhost/openstackdb"
)

const (
	Namespace = "openstack"
	Subsystem = "sharev2"
)

func RegisterCollectors(registry *prometheus.Registry, databaseURL string, projectResolver *project.Resolver, defaults QuotaDefaults, logger *slog.Logger, queryTimeout time.Duration) {
	if databaseURL == "" {
		logger.Info("Collector not loaded", "service", "manila", "reason", "database URL not configured")
		return
	}

	conn, err := db.Connect(databaseURL)
	if err != nil {
		logger.Error("Failed to connect to database", "service", "manila", "error_type", fmt.Sprintf("%T", err))
		registry.MustRegister(util.NewDownCollector(Namespace, Subsystem))
		registry.MustRegister(util.NewDownCollector(Namespace, Subsystem+"_quota"))
		return
	}

	registry.MustRegister(NewSharesCollector(conn, logger))
	registry.MustRegister(NewQuotasCollector(conn, logger, projectResolver, defaults, queryTimeout))

	logger.Info("Registered collectors", "service", "manila")
}
