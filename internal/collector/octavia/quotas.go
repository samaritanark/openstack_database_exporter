package octavia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	octaviadb "github.com/vexxhost/openstackdb/octavia/db/repositories"
)

// Metric suffix -> Octavia quotas column and usage resource name.
var octaviaQuotaResources = []struct {
	metric string
	column string
}{
	{"loadbalancer", "load_balancer"},
	{"listener", "listener"},
	{"pool", "pool"},
	{"member", "member"},
	{"healthmonitor", "health_monitor"},
	{"l7policy", "l7policy"},
	{"l7rule", "l7rule"},
}

const quotaQueryTimeout = 5 * time.Second

var quotaUpDesc = prometheus.NewDesc(
	prometheus.BuildFQName(Namespace, Subsystem, "quota_up"),
	"Whether Octavia quota collection succeeded (1) or failed (0).", nil, nil,
)

var octaviaQuotaDescs map[string]*prometheus.Desc

func init() {
	octaviaQuotaDescs = make(map[string]*prometheus.Desc, len(octaviaQuotaResources))
	for _, r := range octaviaQuotaResources {
		octaviaQuotaDescs[r.metric] = prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, Subsystem, "quota_"+r.metric),
			"quota_"+r.metric,
			[]string{"tenant", "tenant_id", "domain_id", "type"},
			nil,
		)
	}
}

// QuotasCollector emits limits and rebuilt object usage for every Octavia
// quota resource. Octavia does not expose reserved measurements here.
type QuotasCollector struct {
	db              *sql.DB
	quotas          *octaviadb.QuotaRepository
	logger          *slog.Logger
	projectResolver *project.Resolver
	defaults        QuotaDefaults
	queryTimeout    time.Duration
}

// NewQuotasCollector uses a five-second shared query deadline unless the optional
// timeout is positive. Omitted or non-positive values use the default.
func NewQuotasCollector(db *sql.DB, logger *slog.Logger, projectResolver *project.Resolver, defaults QuotaDefaults, timeout ...time.Duration) *QuotasCollector {
	queryTimeout := quotaQueryTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		queryTimeout = timeout[0]
	}
	if defaults == nil {
		defaults = upstreamQuotaDefaults()
		logger.Warn("No Octavia quota defaults supplied; assuming Octavia 2025.2 upstream unlimited (-1) defaults")
	}
	copyDefaults := make(QuotaDefaults, len(defaults))
	for resource, value := range defaults {
		copyDefaults[resource] = value
	}
	return &QuotasCollector{
		db:     db,
		quotas: octaviadb.NewQuotaRepository(db),
		logger: logger.With(
			"namespace", Namespace,
			"subsystem", Subsystem,
			"collector", "quotas",
		),
		projectResolver: projectResolver,
		defaults:        copyDefaults,
		queryTimeout:    queryTimeout,
	}
}

func (c *QuotasCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- quotaUpDesc
	for _, desc := range octaviaQuotaDescs {
		ch <- desc
	}
}

func (c *QuotasCollector) Collect(ch chan<- prometheus.Metric) {
	if err := c.defaults.Validate(); err != nil {
		c.fail(ch, "validate defaults", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.queryTimeout)
	defer cancel()
	limits, err := c.quotas.GetAll(ctx)
	if err != nil {
		c.fail(ch, "query limits", err)
		return
	}
	usages, err := c.quotas.GetUsage(ctx)
	if err != nil {
		c.fail(ch, "query usage", err)
		return
	}

	byProject := make(map[string]map[string]int32, len(limits))
	projects := make(map[string]struct{})
	for _, row := range limits {
		if row.ProjectID == "" {
			continue
		}
		projects[row.ProjectID] = struct{}{}
		byProject[row.ProjectID] = map[string]int32{
			"loadbalancer":  nullIntOrDefault(row.LoadBalancer, c.defaults["loadbalancer"]),
			"listener":      nullIntOrDefault(row.Listener, c.defaults["listener"]),
			"pool":          nullIntOrDefault(row.Pool, c.defaults["pool"]),
			"member":        nullIntOrDefault(row.Member, c.defaults["member"]),
			"healthmonitor": nullIntOrDefault(row.HealthMonitor, c.defaults["healthmonitor"]),
			"l7policy":      nullIntOrDefault(row.L7policy, c.defaults["l7policy"]),
			"l7rule":        nullIntOrDefault(row.L7rule, c.defaults["l7rule"]),
		}
	}
	usage := make(map[string]map[string]int64)
	for _, row := range usages {
		known := false
		for _, resource := range octaviaQuotaResources {
			if row.Resource == resource.column {
				known = true
				break
			}
		}
		if !known {
			c.fail(ch, "validate usage resource", errors.New("unknown resource identifier"))
			return
		}
		if !row.ProjectID.Valid || row.ProjectID.String == "" {
			continue
		}
		pid := row.ProjectID.String
		projects[pid] = struct{}{}
		if usage[pid] == nil {
			usage[pid] = make(map[string]int64)
		}
		usage[pid][row.Resource] += row.InUse
	}
	// Snapshot the resolver once: idle projects receive defaults and zero usage.
	metadata := c.projectResolver.AllProjects()
	for pid := range metadata {
		if pid != "" {
			projects[pid] = struct{}{}
		}
	}
	ids := make([]string, 0, len(projects))
	for pid := range projects {
		ids = append(ids, pid)
	}
	sort.Strings(ids)
	for _, pid := range ids {
		name, domain := pid, ""
		if info, ok := metadata[pid]; ok {
			if info.Name != "" {
				name = info.Name
			}
			domain = info.DomainID
		}
		for _, resource := range octaviaQuotaResources {
			limit := c.defaults[resource.metric]
			if values := byProject[pid]; values != nil {
				limit = values[resource.metric]
			}
			desc := octaviaQuotaDescs[resource.metric]
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(limit), name, pid, domain, "limit")
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(usage[pid][resource.column]), name, pid, domain, "in_use")
		}
	}
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 1)
}

// A failed collection emits only the success gauge, never partial quota data.
func (c *QuotasCollector) fail(ch chan<- prometheus.Metric, operation string, err error) {
	// Driver error messages may contain connection strings, users or query data.
	// Record the operation and safe error classification rather than raw messages.
	kind := fmt.Sprintf("%T", err)
	if errors.Is(err, context.DeadlineExceeded) {
		kind = "deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		kind = "canceled"
	}
	attrs := []any{"operation", operation, "error_type", kind}
	var dbErr *mysql.MySQLError
	if errors.As(err, &dbErr) {
		attrs = append(attrs, "mysql_error_code", dbErr.Number)
	}
	c.logger.Error("Failed to collect octavia quotas", attrs...)
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 0)
}

func nullIntOrDefault(value sql.NullInt32, fallback int32) int32 {
	if value.Valid {
		return value.Int32
	}
	return fallback
}
