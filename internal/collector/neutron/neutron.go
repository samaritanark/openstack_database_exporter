package neutron

import (
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	"github.com/vexxhost/openstack_database_exporter/internal/util"
	db "github.com/vexxhost/openstackdb"
)

const (
	Namespace = "openstack"
	Subsystem = "neutron"
)

func RegisterCollectors(registry *prometheus.Registry, databaseURL string, projectResolver *project.Resolver, logger *slog.Logger, options ...QuotaOptions) {
	if databaseURL == "" {
		logger.Info("Collector not loaded", "service", "neutron", "reason", "database URL not configured")
		return
	}

	conn, err := db.Connect(databaseURL)
	if err != nil {
		logger.Error("Failed to connect to database", "service", "neutron", "error_type", fmt.Sprintf("%T", err))
		registry.MustRegister(util.NewDownCollector(Namespace, Subsystem))
		registry.MustRegister(NewQuotaCollector(nil, logger, projectResolver, options...))
		return
	}

	registry.MustRegister(NewAgentsCollector(conn, logger))
	registry.MustRegister(NewHARouterAgentPortBindingCollector(conn, logger))
	registry.MustRegister(NewFloatingIPCollector(conn, logger))
	registry.MustRegister(NewNetworkCollector(conn, logger))
	registry.MustRegister(NewPortCollector(conn, logger))
	registry.MustRegister(NewRouterCollector(conn, logger))
	registry.MustRegister(NewSecurityGroupCollector(conn, logger))
	registry.MustRegister(NewSubnetCollector(conn, logger))
	registry.MustRegister(NewQuotaCollector(conn, logger, projectResolver, options...))

	logger.Info("Registered collectors", "service", "neutron")
}
