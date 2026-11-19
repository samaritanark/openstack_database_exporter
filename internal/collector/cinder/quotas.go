package cinder

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vexxhost/openstack_database_exporter/internal/collector/project"
	cinderdb "github.com/vexxhost/openstackdb/cinder/db"
)

var quotaResources = []string{"volumes", "snapshots", "backups", "groups", "gigabytes", "backup_gigabytes", "per_volume_gigabytes"}
var typedResources = []string{"volumes", "snapshots", "gigabytes"}

const quotaQueryTimeout = 5 * time.Second

func isProjectResource(r string) bool {
	for _, resource := range quotaResources {
		if resource == r {
			return true
		}
	}
	return false
}

// QuotaOptions bounds the entire quota SQL read and supplies deployment defaults.
type QuotaOptions struct {
	Defaults     *QuotaDefaults
	QueryTimeout time.Duration
}

type quotaValue struct {
	limit           int32
	inUse, reserved int32
}
type quotaScope struct {
	limits  map[string]int32
	usage   map[string]quotaValue
	skipped map[string]bool
}

func newQuotaScope() *quotaScope {
	return &quotaScope{limits: map[string]int32{}, usage: map[string]quotaValue{}, skipped: map[string]bool{}}
}

type quotaMetric struct {
	desc                  *prometheus.Desc
	resource, measurement string
	typed, capacity       bool
}

func quotaMetrics() []quotaMetric {
	var result []quotaMetric
	add := func(name, resource, measurement string, typed, capacity, states bool) {
		labels := []string{"tenant", "tenant_id", "domain_id"}
		if typed {
			labels = append(labels, "volume_type")
		}
		if states {
			labels = append(labels, "type")
		}
		help := name
		if strings.HasSuffix(resource, "gigabytes") {
			help = "Quota capacity in bytes (source GiB multiplied by 2^30); -1 means unlimited only for limits."
		}
		result = append(result, quotaMetric{prometheus.NewDesc(prometheus.BuildFQName(Namespace, Subsystem, name), help, labels, nil), resource, measurement, typed, capacity})
	}
	for _, r := range []string{"volumes", "snapshots", "backups", "groups", "per_volume_gigabytes"} {
		add("quota_"+strings.ReplaceAll(r, "gigabytes", "bytes"), r, "states", false, false, true)
	}
	for _, scope := range []struct{ label, resource string }{{"volume", "gigabytes"}, {"backup", "backup_gigabytes"}} {
		for _, m := range []struct{ suffix, measurement string }{{"max", "limit"}, {"used", "in_use"}, {"reserved", "reserved"}} {
			add("limits_"+scope.label+"_"+m.suffix+"_bytes", scope.resource, m.measurement, false, true, false)
		}
	}
	for _, r := range []string{"volumes", "snapshots"} {
		add("volume_type_quota_"+r, r, "states", true, false, true)
	}
	add("volume_type_quota_bytes", "gigabytes", "limit", true, true, false)
	add("volume_type_quota_in_use_bytes", "gigabytes", "in_use", true, true, false)
	add("volume_type_quota_reserved_bytes", "gigabytes", "reserved", true, true, false)
	return result
}

var cinderQuotaMetrics = quotaMetrics()
var quotaUpDesc = prometheus.NewDesc(prometheus.BuildFQName(Namespace, Subsystem, "quota_up"), "Whether Cinder quota collection succeeded (1) or failed (0).", nil, nil)

// QuotasCollector reports DbQuotaDriver accounting, never rebuilt object counts.
// All required reads and validation finish before quota values are emitted.
type QuotasCollector struct {
	quotas          *cinderdb.QuotaRepository
	logger          *slog.Logger
	projectResolver *project.Resolver
	defaults        *QuotaDefaults
	queryTimeout    time.Duration
	limitsOnly      bool
}

func NewQuotasCollector(db *sql.DB, logger *slog.Logger, resolver *project.Resolver, options ...QuotaOptions) *QuotasCollector {
	opts := QuotaOptions{QueryTimeout: quotaQueryTimeout}
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.QueryTimeout <= 0 {
		opts.QueryTimeout = quotaQueryTimeout
	}
	if opts.Defaults == nil {
		opts.Defaults = upstreamQuotaDefaults()
		logger.Warn("No Cinder quota defaults supplied; assuming Cinder 2025.2 upstream defaults")
	}
	defaults := &QuotaDefaults{Limits: map[string]int32{}, UseDefaultQuotaClass: opts.Defaults.UseDefaultQuotaClass}
	for r, v := range opts.Defaults.Limits {
		defaults.Limits[r] = v
	}
	return &QuotasCollector{quotas: cinderdb.NewQuotaRepository(db), logger: logger.With("service", "cinder", "collector", "quotas"), projectResolver: resolver, defaults: defaults, queryTimeout: opts.QueryTimeout}
}

func (c *QuotasCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- quotaUpDesc
	for _, m := range cinderQuotaMetrics {
		if !c.limitsOnly || m.capacity {
			ch <- m.desc
		}
	}
}

func (c *QuotasCollector) fail(ch chan<- prometheus.Metric, operation string, err error) {
	attrs := append([]any{"operation", operation}, cinderErrorAttrs(err)...)
	c.logger.Error("Failed to collect Cinder quotas", attrs...)
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 0)
}

func (c *QuotasCollector) Collect(ch chan<- prometheus.Metric) {
	if err := c.defaults.Validate(); err != nil {
		c.fail(ch, "validate defaults", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.queryTimeout)
	defer cancel()
	limits, err := c.quotas.GetProjectLimits(ctx)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		c.fail(ch, "query limits", err)
		return
	}
	usage, err := c.quotas.GetProjectUsage(ctx)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		c.fail(ch, "query usage", err)
		return
	}
	var defaults []cinderdb.QuotaGetDefaultClassLimitsRow
	if c.defaults.UseDefaultQuotaClass {
		defaults, err = c.quotas.GetDefaultLimits(ctx)
		if err != nil {
			if ctx.Err() != nil {
				err = errors.Join(err, ctx.Err())
			}
			c.fail(ch, "query defaults", err)
			return
		}
	}
	types, err := c.quotas.GetVolumeTypes(ctx)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		c.fail(ch, "query volume types", err)
		return
	}
	// The active type registry defines recognized name-suffixed resources. Keep
	// full names (including underscores); never infer a type by splitting a key.
	typeNames := map[string]bool{}
	ambiguousTypes := map[string]bool{}
	for _, t := range types {
		if !t.Name.Valid || t.Name.String == "" {
			c.logger.Warn("Skipping Cinder volume type", "operation", "validate volume types", "reason", "null or empty name", "volume_type_id", t.ID)
			continue
		}
		name := t.Name.String
		if typeNames[name] || ambiguousTypes[name] {
			delete(typeNames, name)
			ambiguousTypes[name] = true
			c.logger.Warn("Skipping Cinder volume type", "operation", "validate volume types", "reason", "duplicate active name", "volume_type", name)
			continue
		}
		typeNames[name] = true
	}
	split := func(r string) (base, name string, known bool) {
		if isProjectResource(r) {
			return r, "", true
		}
		for _, base := range typedResources {
			prefix := base + "_"
			if strings.HasPrefix(r, prefix) && typeNames[strings.TrimPrefix(r, prefix)] {
				return base, strings.TrimPrefix(r, prefix), true
			}
		}
		return "", "", false
	}
	effective := map[string]int32{}
	for r, v := range c.defaults.Limits {
		effective[r] = v
	}
	for name := range typeNames {
		for _, r := range typedResources {
			effective[r+"_"+name] = -1
		}
	}
	seenDefaults := map[string]bool{}
	invalidDefaults := map[string]bool{}
	for _, row := range defaults {
		if !row.Resource.Valid || row.Resource.String == "" {
			c.logger.Warn("Skipping Cinder quota default", "operation", "validate default identity", "reason", "null or empty resource")
			continue
		}
		r := row.Resource.String
		if _, _, known := split(r); !known {
			continue
		}
		reason := ""
		switch {
		case seenDefaults[r]:
			reason = "duplicate active default"
		case !row.HardLimit.Valid:
			reason = "null default limit"
		case row.HardLimit.Int32 < -1:
			reason = "default limit below -1"
		}
		seenDefaults[r] = true
		if reason != "" {
			invalidDefaults[r] = true
			c.logger.Warn("Skipping dependent Cinder quota limits", "operation", "validate default limit", "reason", reason, "resource", r)
		}
		if invalidDefaults[r] {
			delete(effective, r)
			continue
		}
		effective[r] = row.HardLimit.Int32
	}
	seenLimits := map[[2]string]bool{}
	projects := map[string]*quotaScope{}
	typed := map[string]map[string]*quotaScope{}
	ensure := func(pid, name string) *quotaScope {
		if projects[pid] == nil {
			projects[pid] = newQuotaScope()
		}
		if name == "" {
			return projects[pid]
		}
		if typed[pid] == nil {
			typed[pid] = map[string]*quotaScope{}
		}
		if typed[pid][name] == nil {
			typed[pid][name] = newQuotaScope()
		}
		return typed[pid][name]
	}
	for _, row := range limits {
		if !row.ProjectID.Valid || row.ProjectID.String == "" || row.Resource == "" {
			c.fail(ch, "validate limit identity", &quotaValidationError{reason: "empty project or resource"})
			return
		}
		base, name, known := split(row.Resource)
		if !known {
			continue
		}
		scope := ensure(row.ProjectID.String, name)
		key := [2]string{row.ProjectID.String, row.Resource}
		if seenLimits[key] {
			scope.skipped[base] = true
			c.logger.Warn("Skipping Cinder quota resource with duplicate active limits", "project_id", row.ProjectID.String, "resource", row.Resource)
			continue
		}
		seenLimits[key] = true
		// DbQuotaDriver.get_project_quotas treats a NULL project override as
		// absent and falls through to class/config defaults. A NULL default
		// class value, in contrast, reaches numerical enforcement unchanged.
		if !row.HardLimit.Valid {
			continue
		}
		if row.HardLimit.Int32 < -1 {
			c.fail(ch, "validate limit", &quotaValidationError{reason: "invalid recorded limit"})
			return
		}
		scope.limits[base] = row.HardLimit.Int32
	}
	for _, row := range usage {
		if !row.ProjectID.Valid || row.ProjectID.String == "" || !row.Resource.Valid || row.Resource.String == "" {
			c.fail(ch, "validate usage identity", &quotaValidationError{reason: "empty project or resource"})
			return
		}
		base, name, known := split(row.Resource.String)
		if !known {
			continue
		}
		if base == "per_volume_gigabytes" {
			continue
		} // limit-only; API's zero placeholders are not usage.
		scope := ensure(row.ProjectID.String, name)
		if _, exists := scope.usage[base]; exists {
			scope.skipped[base] = true
			c.logger.Warn("Skipping Cinder quota resource with duplicate active usage", "project_id", row.ProjectID.String, "resource", row.Resource.String)
			continue
		}
		scope.usage[base] = quotaValue{inUse: row.InUse, reserved: row.Reserved}
	}
	metadata := c.projectResolver.AllProjects()
	for pid := range metadata {
		if pid != "" {
			ensure(pid, "")
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
	emit := func(m quotaMetric, pid, name string, scope *quotaScope) {
		// Never replace ambiguous accounting with a default or zero usage.
		if scope != nil && scope.skipped[m.resource] {
			return
		}
		key := m.resource
		if name != "" {
			key += "_" + name
		}
		limit, limitKnown := effective[key]
		v := quotaValue{limit: limit}
		if scope != nil {
			if limit, ok := scope.limits[m.resource]; ok {
				v.limit = limit
				limitKnown = true
			}
			u := scope.usage[m.resource]
			v.inUse = u.inUse
			v.reserved = u.reserved
		}
		l := labels(pid)
		if m.typed {
			l = append(l, name)
		}
		// Invalid shared defaults suppress only dependent limits. Accounting
		// and explicit project overrides remain independently meaningful.
		if !limitKnown && (m.measurement == "limit" || m.measurement == "states") {
			c.logger.Warn("Skipping Cinder quota limit", "operation", "resolve limit", "reason", "invalid shared default", "project_id", pid, "resource", key)
		}
		send := func(value int32, state string) {
			if !limitKnown && (m.measurement == "limit" || state == "limit") {
				return
			}
			values := append([]string(nil), l...)
			if m.measurement == "states" {
				values = append(values, state)
			}
			sample := float64(value)
			if strings.HasSuffix(m.resource, "gigabytes") && (value != -1 || (m.measurement != "limit" && state != "limit")) {
				sample *= 1 << 30
			}
			ch <- prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, sample, values...)
		}
		switch m.measurement {
		case "limit":
			send(v.limit, "")
		case "in_use":
			send(v.inUse, "")
		case "reserved":
			send(v.reserved, "")
		case "states":
			send(v.limit, "limit")
			if m.resource != "per_volume_gigabytes" {
				send(v.inUse, "in_use")
				send(v.reserved, "reserved")
			}
		}
	}
	for _, m := range cinderQuotaMetrics {
		if c.limitsOnly && !m.capacity {
			continue
		}
		if !m.typed {
			for pid, scope := range projects {
				emit(m, pid, "", scope)
			}
			continue
		}
		if m.resource == "gigabytes" && m.measurement == "limit" {
			// Preserve the legacy project's active-type limit series contract. New
			// measurements are restricted to scopes observed in limits or usage.
			for pid := range projects {
				for name := range typeNames {
					emit(m, pid, name, typed[pid][name])
				}
			}
		} else {
			for pid, scopes := range typed {
				for name, scope := range scopes {
					emit(m, pid, name, scope)
				}
			}
		}
	}
	ch <- prometheus.MustNewConstMetric(quotaUpDesc, prometheus.GaugeValue, 1)
}
