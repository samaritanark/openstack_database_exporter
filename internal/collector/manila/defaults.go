package manila

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// QuotaDefaults contains deployment-configured [quota] defaults, keyed by
// resource name. All twelve values are required; -1 is explicitly unlimited.
// These settings are not stored in the OpenStack database.
type QuotaDefaults map[string]int32

func (d QuotaDefaults) Validate() error {
	known := make(map[string]bool, len(manilaQuotaResources))
	for _, r := range manilaQuotaResources {
		known[r.name] = true
		value, ok := d[r.name]
		if !ok {
			return fmt.Errorf("manila quota default %q is required", r.name)
		}
		if value < -1 {
			return fmt.Errorf("manila quota default %q must be -1 or non-negative", r.name)
		}
	}
	for resource := range d {
		if !known[resource] {
			return fmt.Errorf("unknown manila quota resource %q", resource)
		}
	}
	return nil
}

// LoadQuotaDefaults reads {"manila": {"shares": 50, ...}}.
// No path or an absent Manila section selects the upstream Manila 2025.2 defaults.
func LoadQuotaDefaults(path string, logger *slog.Logger) (QuotaDefaults, error) {
	if path == "" {
		return upstreamQuotaDefaults(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var services map[string]json.RawMessage
	if err := json.Unmarshal(data, &services); err != nil {
		return nil, fmt.Errorf("parsing quota defaults: %w", err)
	}
	raw, ok := services["manila"]
	if !ok {
		logger.Warn("No Manila section in quota defaults file; assuming Manila 2025.2 upstream defaults, which may differ from customized manila.conf settings", "path", path)
		return upstreamQuotaDefaults(), nil
	}
	var values map[string]*int32
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("parsing manila quota defaults: %w", err)
	}
	defaults := make(QuotaDefaults)
	for resource, value := range values {
		if value == nil {
			return nil, fmt.Errorf("manila quota default %q cannot be null", resource)
		}
		defaults[resource] = *value
	}
	if err := defaults.Validate(); err != nil {
		return nil, err
	}
	return defaults, nil
}

// upstreamQuotaDefaults returns a fresh copy of the Manila 2025.2 [quota]
// defaults. Unlike Octavia, most Manila resources have finite defaults.
func upstreamQuotaDefaults() QuotaDefaults {
	return QuotaDefaults{
		"shares": 50, "gigabytes": 1000, "snapshots": 50,
		"snapshot_gigabytes": 1000, "share_networks": 10,
		"share_replicas": 100, "replica_gigabytes": 1000,
		"share_groups": 50, "share_group_snapshots": 50,
		"backups": 10, "backup_gigabytes": 1000, "per_share_gigabytes": -1,
	}
}
