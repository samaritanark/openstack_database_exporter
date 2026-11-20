package collector

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewRegistry_ManilaQuotaQueryTimeout(t *testing.T) {
	address := stalledQuotaServer(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const timeout = 75 * time.Millisecond
	registry := NewRegistry(Config{ManilaDatabaseURL: "mysql://test@" + address + "/manila", ManilaQuotaQueryTimeout: timeout}, logger)
	start := time.Now()
	families, err := registry.Gather()
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.GreaterOrEqual(t, elapsed, timeout)
	require.Less(t, elapsed, time.Second)
	found := false
	for _, family := range families {
		if family.GetName() == "openstack_sharev2_quota_up" {
			found = true
			require.EqualValues(t, 0, family.Metric[0].GetGauge().GetValue())
		} else {
			require.False(t, strings.HasPrefix(family.GetName(), "openstack_sharev2_quota_"))
			require.False(t, strings.HasPrefix(family.GetName(), "openstack_sharev2_share_type_quota_"))
		}
	}
	require.True(t, found)
}
