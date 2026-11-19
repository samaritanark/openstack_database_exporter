package main

import (
	"github.com/stretchr/testify/require"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestCinderQuotaConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := loadCinderQuotaConfig("", "missing", -time.Second, logger)
	require.NoError(t, err)
	require.Nil(t, d)
	_, err = loadCinderQuotaConfig("mysql://enabled", "", 0, logger)
	require.Error(t, err)
	_, err = loadCinderQuotaConfig("mysql://enabled", "missing", time.Second, logger)
	require.Error(t, err)
	d, err = loadCinderQuotaConfig("mysql://enabled", "", time.Second, logger)
	require.NoError(t, err)
	require.NoError(t, d.Validate())
}
