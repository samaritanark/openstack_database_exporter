package collector

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewRegistry_CinderQuotaQueryTimeout(t *testing.T) {
	address := stalledQuotaServer(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const timeout = 75 * time.Millisecond
	registry := NewRegistry(Config{CinderDatabaseURL: "mysql://test@" + address + "/cinder", CinderQuotaQueryTimeout: timeout}, logger)
	start := time.Now()
	families, err := registry.Gather()
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.GreaterOrEqual(t, elapsed, timeout)
	require.Less(t, elapsed, time.Second)
	found := false
	for _, family := range families {
		if family.GetName() == "openstack_cinder_quota_up" {
			found = true
			require.EqualValues(t, 0, family.Metric[0].GetGauge().GetValue())
		} else {
			require.False(t, strings.HasPrefix(family.GetName(), "openstack_cinder_quota_"))
			require.False(t, strings.HasPrefix(family.GetName(), "openstack_cinder_volume_type_quota_"))
			require.False(t, strings.HasPrefix(family.GetName(), "openstack_cinder_limits_"))
		}
	}
	require.True(t, found)
}
