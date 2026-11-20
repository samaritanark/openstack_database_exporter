package neutron

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
)

type QuotaDefaults map[string]int32

var requiredResources = []string{"floatingip", "router", "network", "port", "security_group", "security_group_rule"}

func isRequiredResource(r string) bool {
	for _, v := range requiredResources {
		if r == v {
			return true
		}
	}
	return false
}

// Neutron normalizes all negative configured defaults to unlimited (-1).
func (d QuotaDefaults) Validate() error {
	for _, r := range requiredResources {
		if _, ok := d[r]; !ok {
			return &quotaConfigError{reason: "missing key", key: r}
		}
	}
	for r := range d {
		if _, ok := neutronDefaultQuotas[r]; !ok {
			return &quotaConfigError{reason: "unsupported key", key: safeConfigKey(r)}
		}
	}
	return nil
}

func upstreamQuotaDefaults() QuotaDefaults {
	d := QuotaDefaults{}
	for r, v := range neutronDefaultQuotas {
		d[r] = v
	}
	return d
}

// A missing section selects the explicitly logged upstream fallback. A supplied
// section requires six primary defaults and allows three optional secondary defaults.
func LoadQuotaDefaults(path string, logger *slog.Logger) (defaults QuotaDefaults, loadErr error) {
	defer func() {
		if loadErr != nil {
			logQuotaConfigError(logger, "quota.defaults-file", loadErr)
		}
	}()
	var services map[string]json.RawMessage
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &quotaConfigError{reason: "cannot read file"}
		}
		if services, err = decodeDefaultsObject(data); err != nil {
			return nil, err
		}
	}
	raw, provided := services["neutron"]
	if !provided {
		logger.Warn("No Neutron defaults section supplied; assuming Neutron 2025.2 upstream defaults")
		return upstreamQuotaDefaults(), nil
	}
	values, err := decodeDefaultsObject(raw)
	if err != nil {
		return nil, err
	}
	d := QuotaDefaults{}
	for r, rawValue := range values {
		if _, ok := neutronDefaultQuotas[r]; !ok {
			return nil, &quotaConfigError{reason: "unsupported key", key: safeConfigKey(r)}
		}
		var v *int32
		if err := json.Unmarshal(rawValue, &v); err != nil {
			return nil, &quotaConfigError{reason: "invalid value: expected int32 integer", key: r}
		}
		if v == nil {
			return nil, &quotaConfigError{reason: "invalid value: null", key: r}
		}
		d[r] = *v
		if *v < 0 {
			d[r] = -1
		}
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	for r, v := range neutronDefaultQuotas {
		if _, ok := d[r]; !ok {
			d[r] = v
		}
	}
	return d, nil
}

// Reject duplicate JSON keys instead of choosing an arbitrary effective default.
func decodeDefaultsObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, &quotaConfigError{reason: "malformed JSON"}
	}
	if token != json.Delim('{') {
		return nil, &quotaConfigError{reason: "invalid value: expected object"}
	}
	result := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, &quotaConfigError{reason: "malformed JSON"}
		}
		key, ok := token.(string)
		if !ok {
			return nil, &quotaConfigError{reason: "malformed JSON"}
		}
		if _, exists := result[key]; exists {
			return nil, &quotaConfigError{reason: "duplicate key", key: safeConfigKey(key)}
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, &quotaConfigError{reason: "malformed JSON", key: safeConfigKey(key)}
		}
		result[key] = value
	}
	if _, err = decoder.Token(); err != nil {
		return nil, &quotaConfigError{reason: "malformed JSON"}
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, &quotaConfigError{reason: "trailing content"}
	}
	return result, nil
}

// Only known schema keys are safe to log. Unknown keys can contain secrets.
type quotaConfigError struct{ reason, key string }

func (e *quotaConfigError) Error() string { return "invalid Neutron quota configuration: " + e.reason }
func safeConfigKey(key string) string {
	if key == "neutron" {
		return key
	}
	if _, ok := neutronDefaultQuotas[key]; ok {
		return key
	}
	return "[unsupported]"
}
func logQuotaConfigError(logger *slog.Logger, source string, err error) {
	reason, key := "invalid configuration", ""
	var configErr *quotaConfigError
	if errors.As(err, &configErr) {
		reason, key = configErr.reason, configErr.key
	}
	logger.Error("Invalid Neutron quota configuration", "service", "neutron", "source", source, "key", key, "reason", reason)
}
