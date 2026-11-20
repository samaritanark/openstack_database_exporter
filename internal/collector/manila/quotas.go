package manila

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	maniladb "github.com/vexxhost/openstackdb/manila/db"
)

var manilaQuotaResources = []struct {
	name      string
	shareType bool
	limitOnly bool
}{
	{"shares", true, false},
	{"gigabytes", true, false},
	{"snapshots", true, false},
	{"snapshot_gigabytes", true, false},
	{"share_networks", false, false},
	{"share_replicas", true, false},
	{"replica_gigabytes", true, false},
	{"share_groups", false, false},
	{"share_group_snapshots", false, false},
	{"backups", false, false},
	{"backup_gigabytes", false, false},
	{"per_share_gigabytes", false, true},
}

const quotaQueryTimeout = 5 * time.Second

var quotaUpDesc = prometheus.NewDesc(
	prometheus.BuildFQName(Namespace, Subsystem, "quota_up"),
	"Whether Manila quota collection succeeded (1) or failed (0).", nil, nil,
)

var manilaQuotaDescs, manilaShareTypeQuotaDescs = quotaDescriptions()

func quotaDescriptions() (map[string]*prometheus.Desc, map[string]*prometheus.Desc) {
	projects := make(map[string]*prometheus.Desc)
	shareTypes := make(map[string]*prometheus.Desc)
	for _, resource := range manilaQuotaResources {
		name := strings.ReplaceAll(resource.name, "gigabytes", "bytes")
		help := "quota_" + resource.name
		if strings.HasSuffix(resource.name, "gigabytes") {
			help = "Quota capacity in bytes (source GiB multiplied by 2^30); -1 means unlimited only for limits."
		}
		projects[resource.name] = prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, Subsystem, "quota_"+name),
			help,
			[]string{"tenant", "tenant_id", "domain_id", "type"}, nil,
		)
		if resource.shareType {
			if !strings.HasSuffix(resource.name, "gigabytes") {
				help = "share_type_quota_" + resource.name
			}
			shareTypes[resource.name] = prometheus.NewDesc(
				prometheus.BuildFQName(Namespace, Subsystem, "share_type_quota_"+name),
				help,
				[]string{"tenant", "tenant_id", "domain_id", "share_type", "share_type_name", "type"}, nil,
			)
		}
	}
	return projects, shareTypes
}

type quotaUsage struct{ inUse, reserved int64 }
type quotaScope struct {
	invalid       bool // Invalid recorded limits suppress this entire scope.
	limits        map[string]int32
	usage         map[string]quotaUsage
	shareTypeName string
}

func newQuotaScope() *quotaScope {
	return &quotaScope{limits: make(map[string]int32), usage: make(map[string]quotaUsage)}
}

type shareTypeKey struct{ projectID, shareTypeID string }

// QuotasCollector reports provisioned quota accounting, including outstanding
// reservations, as maintained by Manila's DbQuotaDriver. It does not measure
// physical backend consumption or combine user and share-type accounting.
type QuotasCollector struct {
	quotas          *maniladb.Queries
	logger          *slog.Logger
	projectResolver *project.Resolver
	defaults        QuotaDefaults
	queryTimeout    time.Duration
}

// NewQuotasCollector copies defaults and bounds all quota reads with a shared
// five-second deadline. A positive timeout overrides that deadline.
func NewQuotasCollector(db *sql.DB, logger *slog.Logger, resolver *project.Resolver, defaults QuotaDefaults, timeout ...time.Duration) *QuotasCollector {
	deadline := quotaQueryTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		deadline = timeout[0]
	}
	if defaults == nil {
		defaults = upstreamQuotaDefaults()
		logger.Warn("No Manila quota defaults supplied; assuming Manila 2025.2 upstream defaults")
	}
	copied := make(QuotaDefaults, len(defaults))
	for resource, value := range defaults {
		copied[resource] = value
	}
	return &QuotasCollector{
		quotas:          maniladb.New(db),
		logger:          logger.With("namespace", Namespace, "subsystem", Subsystem, "collector", "quotas"),
		projectResolver: resolver, defaults: copied, queryTimeout: deadline,
	}
}

func (c *QuotasCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- quotaUpDesc
	for _, desc := range manilaQuotaDescs {
		ch <- desc
	}
	for _, desc := range manilaShareTypeQuotaDescs {
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
	defaults, err := c.quotas.QuotaClassGetDefault(ctx)
	if err != nil {
		c.fail(ch, "query default limits", err)
		return
	}
	limits, err := c.quotas.QuotaGetAllByProject(ctx)
	if err != nil {
		c.fail(ch, "query project limits", err)
		return
	}
	// Required for discovery of projects with only user-specific quota records.
	// Per-user enforcement limits are not exported or used as project limits.
	users, err := c.quotas.QuotaGetAllByProjectAndUser(ctx)
	if err != nil {
		c.fail(ch, "query user limits", err)
		return
	}
	usages, err := c.quotas.QuotaUsageGetAllByProject(ctx)
	if err != nil {
		c.fail(ch, "query project usage", err)
		return
	}
	typeLimits, err := c.quotas.QuotaGetAllByProjectAndShareType(ctx)
	if err != nil {
		c.fail(ch, "query share-type limits", err)
		return
	}
	typeUsages, err := c.quotas.QuotaUsageGetAllByProjectAndShareType(ctx)
	if err != nil {
		c.fail(ch, "query share-type usage", err)
		return
	}

	effectiveDefaults := make(QuotaDefaults, len(c.defaults))
	for resource, value := range c.defaults {
		effectiveDefaults[resource] = value
	}
	for _, row := range defaults {
		if _, tracked := effectiveDefaults[row.Resource.String]; tracked && row.Resource.Valid {
			if !validRecordedLimit(row.HardLimit) {
				c.fail(ch, "validate default limit", errors.New("invalid persisted quota limit"))
				return
			}
			effectiveDefaults[row.Resource.String] = row.HardLimit.Int32
		}
	}
	projects := make(map[string]*quotaScope)
	ensureProject := func(pid string) *quotaScope {
		if projects[pid] == nil {
			projects[pid] = newQuotaScope()
		}
		return projects[pid]
	}
	types := make(map[shareTypeKey]*quotaScope)
	ensureType := func(pid, tid, name string) *quotaScope {
		ensureProject(pid)
		key := shareTypeKey{pid, tid}
		if types[key] == nil {
			types[key] = newQuotaScope()
		}
		types[key].shareTypeName = name
		return types[key]
	}
	complete := true
	for _, row := range limits {
		if row.ProjectID.Valid && row.ProjectID.String != "" {
			scope := ensureProject(row.ProjectID.String)
			if _, tracked := manilaQuotaDescs[row.Resource.String]; tracked {
				if !validRecordedLimit(row.HardLimit) {
					c.logger.Error("Invalid persisted Manila project limit; omitting affected scope")
					scope.invalid = true
					complete = false
					continue
				}
				scope.limits[row.Resource.String] = row.HardLimit.Int32
			}
		}
	}
	// A user-only project must be visible, but user overrides never determine
	// its project limits. No per-user metric families are exported here.
	for _, row := range users {
		if row.ProjectID != "" {
			ensureProject(row.ProjectID)
		}
	}
	for _, row := range usages {
		if row.ProjectID.Valid && row.ProjectID.String != "" {
			// This query excludes share-type accounting. Sum users here,
			// treating each NULL component as zero, as the former SQL totals did.
			scope := ensureProject(row.ProjectID.String)
			usage := scope.usage[row.Resource.String]
			if row.InUse.Valid {
				usage.inUse += int64(row.InUse.Int32)
			}
			if row.Reserved.Valid {
				usage.reserved += int64(row.Reserved.Int32)
			}
			scope.usage[row.Resource.String] = usage
		}
	}
	for _, row := range typeLimits {
		// A NULL joined name also represents a deleted/missing type. Omit
		// these type scopes without failing the scrape; an empty valid name
		// is still included. Project accounting is independent of this filter.
		if row.ProjectID != "" && row.ShareTypeID != "" && row.ShareTypeName.Valid {
			scope := ensureType(row.ProjectID, row.ShareTypeID, row.ShareTypeName.String)
			if _, tracked := manilaShareTypeQuotaDescs[row.Resource]; tracked {
				if !validRecordedLimit(row.HardLimit) {
					c.logger.Error("Invalid persisted Manila share-type limit; omitting affected scope")
					scope.invalid = true
					complete = false
					continue
				}
				scope.limits[row.Resource] = row.HardLimit.Int32
			}
		}
	}
	for _, row := range typeUsages {
		if row.ProjectID.Valid && row.ProjectID.String != "" && row.ShareTypeID.Valid && row.ShareTypeID.String != "" && row.ShareTypeName.Valid {
			scope := ensureType(row.ProjectID.String, row.ShareTypeID.String, row.ShareTypeName.String)
			usage := scope.usage[row.Resource.String]
			if row.InUse.Valid {
				usage.inUse += int64(row.InUse.Int32)
			}
			if row.Reserved.Valid {
				usage.reserved += int64(row.Reserved.Int32)
			}
			scope.usage[row.Resource.String] = usage
		}
	}
	metadata := c.projectResolver.AllProjects()
	for pid := range metadata {
		if pid != "" {
			ensureProject(pid)
		}
	}
	labels := func(pid string) []string {
		name, domain := pid, ""
		if info, ok := metadata[pid]; ok {
			if info.Name != "" {
				name = info.Name
			}
			domain = info.DomainID
		}
		return []string{name, pid, domain}
	}
	ids := make([]string, 0, len(projects))
	for pid := range projects {
		ids = append(ids, pid)
	}
	sort.Strings(ids)
	for _, pid := range ids {
		scope := projects[pid]
		if scope.invalid {
			continue
		}
		for _, resource := range manilaQuotaResources {
			limit := effectiveDefaults[resource.name]
			if override, ok := scope.limits[resource.name]; ok {
				limit = override
			}
			emitQuota(ch, manilaQuotaDescs[resource.name], limit, scope.usage[resource.name], resource.limitOnly, strings.HasSuffix(resource.name, "gigabytes"), labels(pid))
		}
	}
	keys := make([]shareTypeKey, 0, len(types))
	for key := range types {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].projectID == keys[j].projectID {
			return keys[i].shareTypeID < keys[j].shareTypeID
		}
		return keys[i].projectID < keys[j].projectID
	})
	for _, key := range keys {
		scope := types[key]
		// Type limits inherit project limits, so an invalid parent suppresses
		// its type scopes too, even when some resources have type overrides.
		if scope.invalid || projects[key.projectID].invalid {
			continue
		}
		typeLabels := append(labels(key.projectID), key.shareTypeID, scope.shareTypeName)
		for _, resource := range manilaQuotaResources {
			if !resource.shareType {
				continue
			}
			limit := effectiveDefaults[resource.name]
			if projectLimit, ok := projects[key.projectID].limits[resource.name]; ok {
				limit = projectLimit
			}
			if override, ok := scope.limits[resource.name]; ok {
				limit = override
			}
			emitQuota(ch, manilaShareTypeQuotaDescs[resource.name], limit, scope.usage[resource.name], false, strings.HasSuffix(resource.name, "gigabytes"), typeLabels)
		}
	}
	up := float64(1)
	if !complete {
		up = 0
	}
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, up)
}

// Manila's API accepts numeric limits >= -1. Although old model comments
// describe NULL as unlimited, the 2025.2 enforcement code compares limits
// numerically without normalizing NULL. Fail instead of guessing its meaning.
func validRecordedLimit(value sql.NullInt32) bool {
	return value.Valid && value.Int32 >= -1
}

func emitQuota(ch chan<- prometheus.Metric, desc *prometheus.Desc, limit int32, usage quotaUsage, limitOnly, capacity bool, labels []string) {
	factor := float64(1)
	if capacity {
		factor = 1 << 30
	}
	limitValue := float64(limit)
	if limit != -1 {
		limitValue *= factor
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, limitValue, append(labels, "limit")...)
	if !limitOnly {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(usage.inUse)*factor, append(labels, "in_use")...)
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(usage.reserved)*factor, append(labels, "reserved")...)
	}
}

// Emit no quota data on failure. Raw driver messages can expose credentials.
func (c *QuotasCollector) fail(ch chan<- prometheus.Metric, operation string, err error) {
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
	c.logger.Error("Failed to collect manila quotas", attrs...)
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 0)
}
