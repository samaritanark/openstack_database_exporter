package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	keystonedb "github.com/vexxhost/openstackdb/keystone/db"
)

func TestContextSnapshotRetainsCacheOnMetadataTimeout(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	}()
	mock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).AddRow("p", "name", "", true, "domain", "", false, ""))
	r := NewResolver(slog.New(slog.NewTextHandler(io.Discard, nil)), keystonedb.New(db), time.Nanosecond)
	mock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillDelayFor(time.Second).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	snapshot := r.AllProjectsContext(ctx)
	require.Less(t, time.Since(start), 500*time.Millisecond)
	require.Equal(t, Info{Name: "name", DomainID: "domain"}, snapshot["p"])
	snapshot["p"] = Info{}
	require.Equal(t, "name", r.projects["p"].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestConcurrentSnapshotsAndRefresh(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1) // Keep sqlmock connection lifecycle deterministic under concurrent scrapes.
	mock.MatchExpectationsInOrder(false)
	const readers = 12
	for range readers + 1 {
		mock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "enabled", "domain_id", "parent_id", "is_domain", "tags"}).AddRow("p", "name", "", true, "domain", "", false, ""))
	}
	r := NewResolver(slog.New(slog.NewTextHandler(io.Discard, nil)), keystonedb.New(db), time.Hour)
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			r.refreshContext(t.Context())
			for range 10 {
				snapshot := r.AllProjectsContext(t.Context())
				snapshot["p"] = Info{Name: "caller mutation"}
				delete(r.AllProjects(), "p")
				name, domain := r.Resolve("p")
				if name != "name" || domain != "domain" {
					t.Errorf("cache changed: %q %q", name, domain)
				}
				name, domain = r.Resolve("unknown")
				if name != "unknown" || domain != "" {
					t.Errorf("invalid owner fallback: %q %q", name, domain)
				}
			}
		})
	}
	wg.Wait()
	require.Equal(t, Info{Name: "name", DomainID: "domain"}, r.AllProjects()["p"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMetadataErrorsAreSanitized(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	mock.ExpectQuery(regexp.QuoteMeta(keystonedb.ListProjects)).WillReturnError(errors.New("synthetic-private-detail"))
	var logs bytes.Buffer
	r := NewResolver(slog.New(slog.NewTextHandler(&logs, nil)), keystonedb.New(db), time.Hour)
	require.NotContains(t, logs.String(), "synthetic-private-detail")
	require.Contains(t, logs.String(), "retaining cached metadata")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Empty(t, r.AllProjectsContext(ctx))
	require.NoError(t, mock.ExpectationsWereMet())
}
