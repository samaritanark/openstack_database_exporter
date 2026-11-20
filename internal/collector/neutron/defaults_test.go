package neutron

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadQuotaDefaults(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		valid         bool
	}{
		{"absent", "{}", true},
		{"other service", "{\"cinder\":{}}", true},
		{"configured", `{"neutron":{"floatingip":0,"router":-5,"network":100,"port":500,"security_group":10,"security_group_rule":100}}`, true},
		{"incomplete", `{"neutron":{"port":3}}`, false},
		{"null section", `{"neutron":null}`, false},
		{"null default", `{"neutron":{"port":null}}`, false},
		{"fraction", `{"neutron":{"port":1.2}}`, false},
		{"overflow", `{"neutron":{"port":2147483648}}`, false},
		{"invalid json", `{`, false},
		{"duplicate section", `{"neutron":{},"neutron":{}}`, false},
		{"duplicate value", `{"neutron":{"floatingip":50,"router":10,"network":100,"port":500,"port":20,"security_group":10,"security_group_rule":100}}`, false},
		{"null document", `null`, false},
		{"unknown", `{"neutron":{"floatingip":0,"router":1,"network":2,"port":3,"security_group":4,"security_group_rule":5,"extra":6}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "defaults.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0600))
			d, err := LoadQuotaDefaults(path, testLogger())
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NoError(t, d.Validate())
			if tt.name == "configured" {
				require.Zero(t, d["floatingip"])
				require.Equal(t, int32(-1), d["router"])
			} else {
				require.Equal(t, upstreamQuotaDefaults(), d)
			}
		})
	}
	_, err := LoadQuotaDefaults(filepath.Join(t.TempDir(), "missing"), testLogger())
	require.Error(t, err)
	d, err := LoadQuotaDefaults("", testLogger())
	require.NoError(t, err)
	require.Equal(t, upstreamQuotaDefaults(), d)
}

func TestOptionalQuotaDefaults(t *testing.T) {
	base := `"floatingip":50,"router":10,"network":100,"port":500,"security_group":10,"security_group_rule":100`
	for _, tt := range []struct {
		name, extra string
		want        QuotaDefaults
	}{
		{"six keys", "", QuotaDefaults{"subnet": 100, "subnetpool": -1, "rbac_policy": 10}},
		{"configured", `,"subnet":21,"subnetpool":22,"rbac_policy":23`, QuotaDefaults{"subnet": 21, "subnetpool": 22, "rbac_policy": 23}},
		{"zero and unlimited", `,"subnet":0,"subnetpool":-1,"rbac_policy":-20`, QuotaDefaults{"subnet": 0, "subnetpool": -1, "rbac_policy": -1}},
		{"one optional", `,"subnet":9`, QuotaDefaults{"subnet": 9, "subnetpool": -1, "rbac_policy": 10}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "defaults.json")
			require.NoError(t, os.WriteFile(path, []byte(`{"neutron":{`+base+tt.extra+`}}`), 0600))
			d, err := LoadQuotaDefaults(path, testLogger())
			require.NoError(t, err)
			for r, v := range tt.want {
				require.Equal(t, v, d[r], r)
			}
		})
	}
	for _, r := range []string{"subnet", "subnetpool", "rbac_policy"} {
		for _, value := range []string{`null`, `1.5`, `"secret"`, `2147483648`, `true`, `[]`, `{}`} {
			t.Run(r+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "defaults.json")
				require.NoError(t, os.WriteFile(path, []byte(`{"neutron":{`+base+`,"`+r+`":`+value+`}}`), 0600))
				_, err := LoadQuotaDefaults(path, testLogger())
				require.Error(t, err)
			})
		}
	}
}

func TestQuotaDefaultsSafeDiagnostics(t *testing.T) {
	for _, tt := range []struct{ content, reason, key string }{
		{`{"neutron":{"port":5}}`, "missing key", "floatingip"},
		{`{"neutron":{"mysql://secret:password@host":1}}`, "unsupported key", "[unsupported]"},
		{`{"neutron":{"port":1,"port":2}}`, "duplicate key", "port"},
		{`{"neutron":{},"neutron":{}}`, "duplicate key", "neutron"},
		{`{"neutron":{"port":"mysql://secret:password@host"}}`, "invalid value: expected int32 integer", "port"},
		{`{"neutron":{"port":null}}`, "invalid value: null", "port"},
		{`{"neutron":{"port":`, "malformed JSON", "neutron"},
		{`{} {"secret":"password"}`, "trailing content", ""},
		{`{} invalid-secret-password`, "trailing content", ""},
	} {
		t.Run(tt.reason+tt.key, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			path := filepath.Join(t.TempDir(), "secret-password.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0600))
			_, err := LoadQuotaDefaults(path, logger)
			require.Error(t, err)
			var entry map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
			require.Equal(t, "neutron", entry["service"])
			require.Equal(t, "quota.defaults-file", entry["source"])
			require.Equal(t, tt.reason, entry["reason"])
			require.Equal(t, tt.key, entry["key"])
			for _, secret := range []string{"secret", "password", "mysql://", tt.content, path} {
				require.NotContains(t, logs.String(), secret)
				require.NotContains(t, err.Error(), secret)
			}
		})
	}
}
