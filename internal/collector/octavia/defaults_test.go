package octavia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadQuotaDefaults(t *testing.T) {
	valid := testDefaults()
	valid["loadbalancer"] = 0
	valid["member"] = -1
	body, err := json.Marshal(map[string]QuotaDefaults{"octavia": valid})
	require.NoError(t, err)
	for name, content := range map[string]string{
		"valid zero and unlimited": string(body),
		"unrelated services":       string(body[:len(body)-1]) + `,"nova":{"other":"settings"},"neutron":[1,2]}`,
		"incomplete":               `{"octavia":{"loadbalancer":0}}`,
		"missing service":          `{}`,
		"unknown service":          `{"nova":{}}`,
		"malformed":                `{`,
		"null default":             `{"octavia":{"loadbalancer":null}}`,
		"fraction":                 `{"octavia":{"loadbalancer":0.5}}`,
		"overflow":                 `{"octavia":{"loadbalancer":2147483648}}`,
		"trailing document":        string(body) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "defaults.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0600))
			got, err := LoadQuotaDefaults(path)
			if name == "valid zero and unlimited" || name == "unrelated services" {
				require.NoError(t, err)
				require.Equal(t, valid, got)
			} else {
				require.Error(t, err)
			}
		})
	}
	got, err := LoadQuotaDefaults("")
	require.NoError(t, err)
	require.NoError(t, got.Validate())
	for _, value := range got {
		require.EqualValues(t, -1, value)
	}
	_, err = LoadQuotaDefaults(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
}

func TestQuotaDefaultsValidation(t *testing.T) {
	for _, scenario := range []string{"missing", "negative", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			d := testDefaults()
			switch scenario {
			case "missing":
				delete(d, "member")
			case "negative":
				d["member"] = -2
			case "unknown":
				d["members"] = 1
			}
			require.Error(t, d.Validate())
		})
	}
}
