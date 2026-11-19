package cinder

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestCinderErrorDiagnostics(t *testing.T) {
	secret := "mysql://private-user:secret-password@private-host/private-db SELECT private_sql"
	tests := []struct {
		name   string
		err    error
		fields []string
	}{
		{"mysql", fmt.Errorf("%s: %w", secret, &mysql.MySQLError{Number: 1045, Message: secret}), []string{"error_category=mysql", "mysql_error_code=1045"}},
		{"deadline", fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), []string{"error_category=timeout", "reason=\"deadline exceeded\""}},
		{"canceled", fmt.Errorf("%s: %w", secret, context.Canceled), []string{"error_category=canceled", "reason=\"context canceled\""}},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 3306}, Err: errors.New(secret)}, []string{"error_category=network"}},
		{"url", &url.Error{Op: "parse", URL: secret, Err: errors.New(secret)}, []string{"error_category=connection_url"}},
		{"connection", fmt.Errorf("%s: %w", secret, sql.ErrConnDone), []string{"error_category=connection"}},
		{"unknown", errors.New(secret), []string{"error_category=unclassified"}},
		{"validation", &quotaValidationError{reason: "invalid recorded limit", resource: "volumes"}, []string{"error_category=validation", "reason=\"invalid recorded limit\"", "resource=volumes"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			logger.Error("Cinder operation failed", append([]any{"operation", "query limits"}, cinderErrorAttrs(tt.err)...)...)
			for _, field := range append(tt.fields, "operation=\"query limits\"", "error_type=") {
				require.Contains(t, logs.String(), field)
			}
			for _, sensitive := range []string{"private-user", "secret-password", "private-host", "private-db", "private_sql", "mysql://", "192.0.2.1", "3306"} {
				require.NotContains(t, logs.String(), sensitive)
			}
		})
	}
}

func TestCinderConnectionLogsDoNotLeakCredentials(t *testing.T) {
	for _, connection := range []string{
		"mysql://private-user:secret-password@private-host/private-db?timeout=secret-option",
		"private-scheme://private-user:secret-password@private-host/private-db",
		"mysql://private-user:secret-password@%zz/private-db",
	} {
		var logs bytes.Buffer
		reg := prometheus.NewRegistry()
		RegisterCollectors(reg, connection, nil, slog.New(slog.NewTextHandler(&logs, nil)))
		require.Contains(t, logs.String(), "operation=connect")
		require.Contains(t, logs.String(), "error_category=")
		for _, sensitive := range []string{"private-user", "secret-password", "private-host", "private-db", "secret-option", "private-scheme", "%zz"} {
			require.NotContains(t, logs.String(), sensitive)
		}
		families, err := reg.Gather()
		require.NoError(t, err)
		require.Len(t, families, 2)
		for _, family := range families {
			require.Zero(t, family.Metric[0].GetGauge().GetValue())
		}
	}
}

func TestQuotaDeadlineLogsContextReason(t *testing.T) {
	db, mock, _, resolver := newQuotaTest(t)
	mock.ExpectQuery(regexp.QuoteMeta(quotaQueries[0])).WillDelayFor(time.Second).WillReturnRows(quotaRows()[0])
	var logs bytes.Buffer
	f := gatherQuotas(t, NewQuotasCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), resolver, QuotaOptions{QueryTimeout: 10 * time.Millisecond}))
	require.Zero(t, quotaSample(t, f, "quota_up", nil))
	require.Len(t, f, 1)
	require.Contains(t, logs.String(), "operation=\"query limits\"")
	require.Contains(t, logs.String(), "error_category=timeout")
	require.Contains(t, logs.String(), "reason=\"deadline exceeded\"")
}

func TestQuotaDefaultsDiagnostics(t *testing.T) {
	d := upstreamQuotaDefaults()
	delete(d.Limits, "volumes")
	require.EqualError(t, d.Validate(), `cinder quota default "volumes": missing default key`)
	d.Limits["volumes"] = -2
	require.EqualError(t, d.Validate(), `cinder quota default "volumes": default must be -1 or non-negative`)

	db, _, _, resolver := newQuotaTest(t)
	var logs bytes.Buffer
	f := gatherQuotas(t, NewQuotasCollector(db, slog.New(slog.NewTextHandler(&logs, nil)), resolver, QuotaOptions{Defaults: &QuotaDefaults{}}))
	require.Zero(t, quotaSample(t, f, "quota_up", nil))
	require.Contains(t, logs.String(), "error_category=validation")
	require.Contains(t, logs.String(), "reason=\"missing default key\"")
	require.Contains(t, logs.String(), "resource=volumes")
}
