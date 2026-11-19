package cinder

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// QuotaDefaults mirrors Cinder's deployment configuration. Type resources have
// no configuration flag and default independently to -1, not the project limit.
type QuotaDefaults struct {
	Limits               map[string]int32
	UseDefaultQuotaClass bool
}

func upstreamQuotaDefaults() *QuotaDefaults {
	return &QuotaDefaults{Limits: map[string]int32{
		"volumes": 10, "snapshots": 10, "backups": 10, "groups": 10,
		"gigabytes": 1000, "backup_gigabytes": 1000, "per_volume_gigabytes": -1,
	}, UseDefaultQuotaClass: true}
}

func (d *QuotaDefaults) Validate() error {
	if d == nil {
		return &quotaValidationError{reason: "defaults are required"}
	}
	for _, r := range quotaResources {
		v, ok := d.Limits[r]
		if !ok {
			return &quotaValidationError{reason: "missing default key", resource: r}
		}
		if v < -1 {
			return &quotaValidationError{reason: "default must be -1 or non-negative", resource: r}
		}
	}
	for r := range d.Limits {
		if !isProjectResource(r) {
			return &quotaValidationError{reason: "unknown quota resource"}
		}
	}
	return nil
}

// LoadQuotaDefaults accepts a complete cinder section alongside existing
// Octavia/Manila sections. use_default_quota_class is optional (defaults true).
func LoadQuotaDefaults(path string, logger *slog.Logger) (*QuotaDefaults, error) {
	fallback := func() *QuotaDefaults {
		logger.Warn("No Cinder quota defaults supplied; assuming Cinder 2025.2 upstream defaults")
		return upstreamQuotaDefaults()
	}
	if path == "" {
		return fallback(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var services map[string]json.RawMessage
	if err := json.Unmarshal(data, &services); err != nil {
		return nil, err
	}
	raw, ok := services["cinder"]
	if !ok {
		return fallback(), nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	d := &QuotaDefaults{Limits: map[string]int32{}, UseDefaultQuotaClass: true}
	for r, raw := range values {
		if string(raw) == "null" {
			return nil, fmt.Errorf("cinder quota setting %q cannot be null", r)
		}
		if r == "use_default_quota_class" {
			if err := json.Unmarshal(raw, &d.UseDefaultQuotaClass); err != nil {
				return nil, err
			}
		} else {
			var v int32
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, err
			}
			d.Limits[r] = v
		}
	}
	return d, d.Validate()
}
