package cinder

import (
	"database/sql"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	"log/slog"
)

// LimitsCollector retains the capacity-only constructor for existing callers.
// Service registration uses one QuotasCollector for all quota metrics, so limits
// and counts share bulk reads and cannot report inconsistent default handling.
type LimitsCollector struct{ *QuotasCollector }

func NewLimitsCollector(db *sql.DB, logger *slog.Logger, resolver *project.Resolver) *LimitsCollector {
	c := NewQuotasCollector(db, logger, resolver)
	c.limitsOnly = true
	return &LimitsCollector{c}
}
