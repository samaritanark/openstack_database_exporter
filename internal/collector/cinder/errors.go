package cinder

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/go-sql-driver/mysql"
)

// quotaValidationError contains only exporter-controlled reasons and known
// resource keys. Never wrap arbitrary database or configuration text in it.
type quotaValidationError struct {
	reason, resource string
}

func (e *quotaValidationError) Error() string {
	if e.resource != "" {
		return fmt.Sprintf("cinder quota default %q: %s", e.resource, e.reason)
	}
	return "cinder quota validation: " + e.reason
}

// cinderErrorAttrs deliberately excludes error messages, SQL, URLs and network
// addresses. Wrapped driver errors may contain credentials or connection data.
func cinderErrorAttrs(err error) []any {
	attrs := []any{"error_type", fmt.Sprintf("%T", err)}
	category := "unclassified"
	var dbErr *mysql.MySQLError
	var validation *quotaValidationError
	var network net.Error
	var urlErr *url.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
		attrs = append(attrs, "reason", "deadline exceeded")
	case errors.Is(err, context.Canceled):
		category = "canceled"
		attrs = append(attrs, "reason", "context canceled")
	case errors.As(err, &validation):
		category = "validation"
		attrs = append(attrs, "reason", validation.reason)
		if validation.resource != "" {
			attrs = append(attrs, "resource", validation.resource)
		}
	case errors.As(err, &dbErr):
		category = "mysql"
	case errors.Is(err, sql.ErrConnDone), errors.Is(err, driver.ErrBadConn):
		category = "connection"
	case errors.As(err, &urlErr):
		category = "connection_url"
	case errors.As(err, &network):
		category = "network"
		if network.Timeout() {
			category = "timeout"
			attrs = append(attrs, "reason", "network timeout")
		}
	}
	if errors.As(err, &dbErr) {
		attrs = append(attrs, "mysql_error_code", dbErr.Number)
	}
	return append(attrs, "error_category", category)
}
