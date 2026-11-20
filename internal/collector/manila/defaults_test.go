package manila

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadQuotaDefaults(t *testing.T) {
	valid := upstreamQuotaDefaults()
	valid["shares"] = 0
	valid["share_replicas"] = -1
	body, err := json.Marshal(map[string]QuotaDefaults{"manila": valid})
	require.NoError(t, err)
	for name, content := range map[string]string{
		"valid zero and unlimited": string(body),
		"unrelated services":       string(body[:len(body)-1]) + `,"nova":{"other":"settings"},"neutron":[1,2]}`,
		"incomplete":               `{"manila":{"shares":0}}`,
		"missing service":          `{}`,
		"null section":             `{"manila":null}`,
		"malformed section":        `{"manila":[]}`,
		"negative default":         `{"manila":{"shares":-2}}`,
		"unknown service":          `{"nova":{}}`,
		"malformed":                `{`,
		"null default":             `{"manila":{"shares":null}}`,
		"fraction":                 `{"manila":{"shares":0.5}}`,
		"overflow":                 `{"manila":{"shares":2147483648}}`,
		"trailing document":        string(body) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "defaults.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0600))
			got, err := LoadQuotaDefaults(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
			switch name {
			case "valid zero and unlimited", "unrelated services":
				require.NoError(t, err)
				require.Equal(t, valid, got)
			case "missing service", "unknown service":
				require.NoError(t, err)
				require.Equal(t, upstreamQuotaDefaults(), got)
			default:
				require.Error(t, err)
			}
		})
	}
	got, err := LoadQuotaDefaults("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.NoError(t, got.Validate())
	require.Equal(t, upstreamQuotaDefaults(), got)
	require.EqualValues(t, 50, got["shares"])
	require.EqualValues(t, 100, got["share_replicas"])
	require.EqualValues(t, -1, got["per_share_gigabytes"])
	_, err = LoadQuotaDefaults(filepath.Join(t.TempDir(), "absent"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
}

func TestQuotaDefaultsValidation(t *testing.T) {
	for _, scenario := range []string{"missing", "negative", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			d := upstreamQuotaDefaults()
			switch scenario {
			case "missing":
				delete(d, "share_replicas")
			case "negative":
				d["share_replicas"] = -2
			case "unknown":
				d["share_replicass"] = 1
			}
			require.Error(t, d.Validate())
		})
	}
}
