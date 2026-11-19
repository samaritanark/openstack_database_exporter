package octavia

import (
	"encoding/json"
	"fmt"
	"os"
)

// QuotaDefaults contains deployment-configured [quotas] defaults, keyed by
// metric suffix. All seven values are required; -1 is explicitly unlimited.
// These settings are not stored in the OpenStack database.
type QuotaDefaults map[string]int32

func (d QuotaDefaults) Validate() error {
	known := make(map[string]bool, len(octaviaQuotaResources))
	for _, r := range octaviaQuotaResources {
		known[r.metric] = true
		value, ok := d[r.metric]
		if !ok {
			return fmt.Errorf("octavia quota default %q is required", r.metric)
		}
		if value < -1 {
			return fmt.Errorf("octavia quota default %q must be -1 or non-negative", r.metric)
		}
	}
	for resource := range d {
		if !known[resource] {
			return fmt.Errorf("unknown octavia quota resource %q", resource)
		}
	}
	return nil
}

// LoadQuotaDefaults reads {"octavia": {"loadbalancer": 10, ...}}.
// No path selects upstream Octavia 2025.2 unlimited defaults.
func LoadQuotaDefaults(path string) (QuotaDefaults, error) {
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
	var values map[string]*int32
	if raw, ok := services["octavia"]; ok {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("parsing octavia quota defaults: %w", err)
		}
	}
	defaults := make(QuotaDefaults)
	for resource, value := range values {
		if value == nil {
			return nil, fmt.Errorf("octavia quota default %q cannot be null", resource)
		}
		defaults[resource] = *value
	}
	if err := defaults.Validate(); err != nil {
		return nil, err
	}
	return defaults, nil
}

// upstreamQuotaDefaults returns a fresh map so callers cannot change defaults
// for other collectors. Octavia 2025.2 defaults every resource to unlimited.
func upstreamQuotaDefaults() QuotaDefaults {
	defaults := make(QuotaDefaults, len(octaviaQuotaResources))
	for _, resource := range octaviaQuotaResources {
		defaults[resource.metric] = -1
	}
	return defaults
}
