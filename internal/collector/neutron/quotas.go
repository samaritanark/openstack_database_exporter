package neutron

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	neutrondb "github.com/vexxhost/openstackdb/neutron/db"
)

// Neutron default quota values per resource type.
var neutronDefaultQuotas = map[string]int32{
	"floatingip":          50,
	"network":             100,
	"port":                500,
	"rbac_policy":         10,
	"router":              10,
	"security_group":      10,
	"security_group_rule": 100,
	"subnet":              100,
	"subnetpool":          -1,
}

// quotaResources is the ordered list of resource types to emit.
var quotaResources = []string{
	"floatingip",
	"network",
	"port",
	"rbac_policy",
	"router",
	"security_group",
	"security_group_rule",
	"subnet",
	"subnetpool",
}

var quotaDescs map[string]*prometheus.Desc

func init() {
	quotaDescs = make(map[string]*prometheus.Desc, len(quotaResources))
	for _, r := range quotaResources {
		quotaDescs[r] = prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, Subsystem, "quota_"+r),
			"quota_"+r,
			[]string{"tenant", "tenant_id", "domain_id", "type"},
			nil,
		)
	}
}

// QuotaOptions applies to DbQuotaNoLockDriver accounting only.
type QuotaOptions struct {
	Defaults     QuotaDefaults
	QueryTimeout time.Duration
	// ConfigError prevents collection when loading global defaults failed.
	ConfigError error
}

var quotaUpDesc = prometheus.NewDesc(prometheus.BuildFQName(Namespace, Subsystem, "quota_up"), "Whether Neutron quota collection succeeded (1) or failed (0).", nil, nil)

var quotaSkippedDesc = prometheus.NewDesc(prometheus.BuildFQName(Namespace, Subsystem, "quota_skipped_records"), "Malformed input records observed during a successful Neutron quota collection (aggregated rows count once).", nil, nil)

type QuotaCollector struct {
	queries         *neutrondb.Queries
	logger          *slog.Logger
	projectResolver *project.Resolver
	defaults        QuotaDefaults
	timeout         time.Duration
	configError     error
}

func NewQuotaCollector(db *sql.DB, logger *slog.Logger, resolver *project.Resolver, options ...QuotaOptions) *QuotaCollector {
	opts := QuotaOptions{QueryTimeout: 5 * time.Second}
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.QueryTimeout == 0 {
		opts.QueryTimeout = 5 * time.Second
	}
	if opts.Defaults == nil && opts.ConfigError == nil {
		opts.Defaults = upstreamQuotaDefaults()
		logger.Warn("No Neutron defaults supplied; assuming Neutron 2025.2 upstream defaults and DbQuotaNoLockDriver")
	}
	defaults := make(QuotaDefaults, len(opts.Defaults))
	for r, v := range opts.Defaults {
		if v < 0 {
			v = -1
		}
		defaults[r] = v
	}
	var queries *neutrondb.Queries
	if db != nil {
		queries = neutrondb.New(db)
	}
	if opts.ConfigError == nil {
		if opts.QueryTimeout < 0 {
			opts.ConfigError = &quotaConfigError{reason: "query timeout must be positive", key: "neutron.quota-query-timeout"}
		} else {
			opts.ConfigError = defaults.Validate()
		}
		if opts.ConfigError != nil {
			logQuotaConfigError(logger, "collector options", opts.ConfigError)
		}
	}
	return &QuotaCollector{queries: queries, logger: logger.With("service", "neutron", "collector", "quotas"), projectResolver: resolver, defaults: defaults, timeout: opts.QueryTimeout, configError: opts.ConfigError}
}

func (c *QuotaCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- quotaUpDesc
	ch <- quotaSkippedDesc
	for _, desc := range quotaDescs {
		ch <- desc
	}
}

// Never log driver error text: it may contain credentials or a connection URL.
func (c *QuotaCollector) fail(ch chan<- prometheus.Metric, operation string, err error) {
	c.logger.Error("Failed to collect Neutron quotas", "operation", operation, "error_type", fmt.Sprintf("%T", err))
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 0)
}

type quotaKey struct{ project, resource string }

func (c *QuotaCollector) Collect(ch chan<- prometheus.Metric) {
	if c.configError != nil {
		ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 0)
		return
	}
	if c.timeout <= 0 || c.queries == nil {
		c.fail(ch, "database or timeout", fmt.Errorf("unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	// All SQL reads share this deadline. The reservation query evaluates one
	// inclusive UTC cutoff on the database clock for all requested resources.
	limits, err := c.queries.GetQuotas(ctx)
	if err != nil {
		c.fail(ch, "limits", err)
		return
	}
	counts, err := c.queries.GetResourceCountsByProject(ctx)
	if err != nil {
		c.fail(ch, "usage", err)
		return
	}
	reservations, err := c.queries.GetReservationsForResources(ctx, quotaResources)
	if err != nil {
		c.fail(ch, "reservations", err)
		return
	}
	projects := map[string]project.Info{}
	// Validate all records before emission. Poisoned scopes cannot be restored
	// by later rows. Missing identifiers broaden omission only as far as necessary.
	lm := map[quotaKey]int32{}
	um := map[quotaKey]int64{}
	rm := map[quotaKey]int64{}
	bad := map[quotaKey]bool{}
	badProjects := map[string]bool{}
	badResources := map[string]bool{}
	badAll := false
	skipped := 0
	logged := map[string]bool{}
	logReason := func(reason string, k quotaKey) {
		if logged[reason] {
			return
		}
		logged[reason] = true
		// Resource names are allowlisted; hash owners rather than logging raw DB data.
		resource := k.resource
		if _, ok := quotaDescs[resource]; !ok {
			resource = "[unknown]"
		}
		c.logger.Warn("Omitting Neutron quota data", "reason", reason, "resource", resource, "owner_hash", fmt.Sprintf("%x", sha256.Sum256([]byte(k.project)))[:12])
	}
	invalidate := func(reason string, k quotaKey) {
		skipped++
		logReason(reason, k)
		owner := strings.TrimSpace(k.project) != ""
		_, resource := quotaDescs[k.resource]
		switch {
		case owner && resource:
			bad[k] = true
		case owner:
			badProjects[k.project] = true
		case resource:
			badResources[k.resource] = true
		default:
			badAll = true
		}
	}
	addProject := func(pid string) {
		if strings.TrimSpace(pid) != "" {
			if _, ok := projects[pid]; !ok {
				projects[pid] = project.Info{Name: pid}
			}
		}
	}
	seenLimits := map[quotaKey]bool{}
	for _, q := range limits {
		k := quotaKey{q.ProjectID.String, q.Resource.String}
		addProject(k.project)
		if q.Resource.Valid && strings.TrimSpace(k.resource) != "" {
			if _, ok := quotaDescs[k.resource]; !ok {
				logReason("unknown extension resource", k)
				continue
			}
		} else {
			invalidate("missing quota resource", k)
			continue
		}
		if !q.ProjectID.Valid || strings.TrimSpace(k.project) == "" {
			invalidate("missing quota owner", k)
			continue
		}
		duplicate := seenLimits[k]
		seenLimits[k] = true
		if duplicate {
			invalidate("duplicate quota override", k)
			continue
		}
		// Neutron applies stored overrides directly: NULL is not inheritance.
		if !q.Limit.Valid {
			invalidate("NULL quota limit", k)
			continue
		}
		lm[k] = q.Limit.Int32
	}
	seenUsage := map[quotaKey]bool{}
	for _, q := range counts {
		// Explicitly empty owners are Neutron system resources, not tenants.
		// SQL NULL owners must still reach conservative malformed-data handling.
		if q.ProjectID.Valid && q.ProjectID.String == "" {
			continue
		}
		k := quotaKey{q.ProjectID.String, q.Resource}
		addProject(k.project)
		if _, ok := quotaDescs[k.resource]; !ok {
			if strings.TrimSpace(k.resource) == "" {
				invalidate("missing usage resource", k)
			} else {
				logReason("unknown extension resource", k)
			}
			continue
		}
		duplicate := seenUsage[k]
		seenUsage[k] = true
		switch {
		case !q.ProjectID.Valid || strings.TrimSpace(k.project) == "":
			invalidate("missing usage owner", k)
		case duplicate:
			invalidate("duplicate usage aggregate", k)
		case q.Cnt < 0:
			invalidate("negative usage", k)
		default:
			um[k] = q.Cnt
		}
	}
	// The public query returns one raw delta per reservation/resource primary
	// key. Distinct reservations for the same owner/resource must all contribute.
	seenReserved := map[struct{ reservation, resource string }]bool{}
	for _, q := range reservations {
		// Explicitly empty owners are Neutron system resources, not tenants.
		// SQL NULL owners must still reach conservative malformed-data handling.
		if q.ProjectID.Valid && q.ProjectID.String == "" {
			continue
		}
		k := quotaKey{q.ProjectID.String, q.Resource}
		addProject(k.project)
		if _, ok := quotaDescs[k.resource]; !ok {
			if strings.TrimSpace(k.resource) == "" {
				invalidate("missing reservation resource", k)
			} else {
				logReason("unknown extension resource", k)
			}
			continue
		}
		delta := struct{ reservation, resource string }{q.ReservationID, q.Resource}
		duplicate := seenReserved[delta]
		seenReserved[delta] = true
		switch {
		case !q.ProjectID.Valid || strings.TrimSpace(k.project) == "":
			invalidate("missing reservation owner", k)
		case duplicate:
			invalidate("duplicate reservation delta", k)
		case !q.Amount.Valid || q.Amount.Int32 < 0:
			invalidate("invalid reservation amount", k)
		default:
			rm[k] += int64(q.Amount.Int32)
		}
	}
	if err := ctx.Err(); err != nil {
		c.fail(ch, "deadline", err)
		return
	}
	// Metadata is optional. Bound refresh by the remaining deadline, but do not
	// discard successful SQL reads if enrichment exhausts it. The resolver
	// retains stale cached identities on failure.
	if c.projectResolver != nil {
		for pid, info := range c.projectResolver.AllProjectsContext(ctx) {
			projects[pid] = info
		}
	}
	for pid, info := range projects {
		if strings.TrimSpace(pid) == "" {
			continue
		} // Unusable metadata cannot identify a quota owner.
		name := info.Name
		if name == "" {
			name = pid
		}
		for _, r := range quotaResources {
			k := quotaKey{pid, r}
			if badAll || badProjects[pid] || badResources[r] || bad[k] {
				continue
			}
			limit := neutronDefaultQuotas[r]
			if v, ok := c.defaults[r]; ok {
				limit = v
			}
			if v, ok := lm[k]; ok {
				limit = v
			}
			for measurement, value := range map[string]float64{"limit": float64(limit), "used": float64(um[k]), "reserved": float64(rm[k])} {
				ch <- prometheus.MustNewConstMetric(quotaDescs[r], prometheus.GaugeValue, value, name, pid, info.DomainID, measurement)
			}
		}
	}
	ch <- prometheus.MustNewConstMetric(quotaSkippedDesc, prometheus.GaugeValue, float64(skipped))
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 1)
}
