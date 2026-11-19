package cinder

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadQuotaDefaults(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "defaults.json")
	for name, body := range map[string]string{
		"old services": `{"octavia":{"loadbalancer":1},"manila":{"shares":2}}`,
		"complete":     `{"cinder":{"volumes":0,"snapshots":-1,"backups":20,"groups":3,"gigabytes":456,"backup_gigabytes":-1,"per_volume_gigabytes":7,"use_default_quota_class":false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(body), 0600))
			d, err := LoadQuotaDefaults(path, logger)
			require.NoError(t, err)
			if name == "complete" {
				require.EqualValues(t, 0, d.Limits["volumes"])
				require.EqualValues(t, -1, d.Limits["snapshots"])
				require.False(t, d.UseDefaultQuotaClass)
			} else {
				require.Equal(t, upstreamQuotaDefaults(), d)
			}
		})
	}
	for _, body := range []string{`{"cinder":null}`, `{"cinder":{}}`, `{"cinder":{"volumes":null}}`, `{"cinder":{"volumes":-2}}`, `{"cinder":{"volumes":1.1}}`, `{"cinder":{"volumes":2147483648}}`, `{"cinder":{"use_default_quota_class":null}}`, `{"cinder":{"use_default_quota_class":7}}`, `not json`} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0600))
		_, err := LoadQuotaDefaults(path, logger)
		require.Error(t, err, body)
	}
	d := upstreamQuotaDefaults()
	d.Limits["unknown"] = 1
	body, _ := json.Marshal(map[string]any{"cinder": d.Limits})
	require.NoError(t, os.WriteFile(path, body, 0600))
	_, err := LoadQuotaDefaults(path, logger)
	require.Error(t, err)
	_, err = LoadQuotaDefaults(path+"missing", logger)
	require.Error(t, err)
	d, err = LoadQuotaDefaults("", logger)
	require.NoError(t, err)
	require.Equal(t, upstreamQuotaDefaults(), d)
}
