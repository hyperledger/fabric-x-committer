/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package db

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

const (
	// nsIDTemplatePlaceholder is used as a template placeholder for SQL queries.
	nsIDTemplatePlaceholder = "${NAMESPACE_ID}"

	// splitIntoTabletsPlaceholder is replaced with "SPLIT INTO N TABLETS" for YugabyteDB
	// or removed entirely for PostgreSQL (when tablets is 0 or unset).
	splitIntoTabletsPlaceholder = "${SPLIT_INTO_TABLETS}"

	// tableNameTempl is the template for the table name for each namespace.
	tableNameTempl = "ns_" + nsIDTemplatePlaceholder
)

var (
	//go:embed init_database_tmpl.sql
	dbInitSQLStmt string
	//go:embed create_namespace_tmpl.sql
	createNamespaceSQLStmt string

	systemNamespaces = []string{committerpb.MetaNamespaceID, committerpb.ConfigNamespaceID}

	logger = flogging.MustGetLogger("db-init")
)

// NewPool creates a new pool from a database config.
func NewPool(ctx context.Context, config *Config) (*pgxpool.Pool, error) {
	connString, err := config.DataSourceName()
	if err != nil {
		return nil, errors.Wrapf(err, "could not build database connection string")
	}
	logger.Infof("DB source: db=%s, endpoints=%s, load_balance=%t, tls_mode=%s",
		config.Database, config.EndpointsString(), config.LoadBalance, config.TLS.Mode)
	poolConfig, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, errors.Wrapf(err, "failed parsing datasource")
	}

	poolConfig.MaxConns = config.MaxConnections
	poolConfig.MinConns = config.MinConnections

	ConfigureConnReadDeadline(poolConfig)

	return retry.ExecuteWithResult(ctx, config.Retry, func() (*pgxpool.Pool, error) {
		p, poolErr := pgxpool.NewWithConfig(ctx, poolConfig)
		if poolErr != nil {
			return nil, errors.Wrap(poolErr, "failed to create a connection pool")
		}
		// NewWithConfig creates the pool lazily without connecting, so we ping to
		// verify connectivity eagerly and let the retry loop handle transient failures.
		return p, errors.Wrap(p.Ping(ctx), "failed to create a connection pool")
	})
}

// SetupSystemTablesAndNamespaces creates the required system tables and namespaces in the database.
// This function should be called before operating against the database.
// It is safe to run multiple times.
func SetupSystemTablesAndNamespaces(
	ctx context.Context, config *Config,
) error {
	pool, err := NewPool(ctx, config)
	if err != nil {
		return errors.Wrap(err, "failed to connect to database")
	}
	defer pool.Close()

	tablePreSplitTablets, err := GetTablePreSplitTablets(ctx, pool, config)
	if err != nil {
		return errors.Wrap(err, "failed to determine database type")
	}

	logger.Info("Creating tx status table, metadata table, and their methods.")
	if execErr := retry.ExecuteSQL(ctx, config.Retry, pool,
		fmtSplitIntoTablets(dbInitSQLStmt, tablePreSplitTablets)); execErr != nil {
		return fmt.Errorf("failed to create system tables and functions: %w", execErr)
	}

	for _, nsID := range systemNamespaces {
		query := CreateNsTables(nsID, tablePreSplitTablets)
		if execErr := retry.ExecuteSQL(ctx, config.Retry, pool, query); execErr != nil {
			return errors.Wrapf(
				execErr,
				"failed to create table and functions for namespace [%s] with query [%s]", nsID, query,
			)
		}
		logger.Infof("namespace %s: created table and its methods.", nsID)
	}
	return nil
}

// CreateNsTables creates the table and functions for a namespace.
func CreateNsTables(nsID string, tablets int) string {
	query := FmtNsID(createNamespaceSQLStmt, nsID)
	return fmtSplitIntoTablets(query, tablets)
}

// FmtNsID replaces the namespace placeholder with the namespace ID in an SQL template string.
func FmtNsID(sqlTemplate, namespaceID string) string {
	return strings.ReplaceAll(sqlTemplate, nsIDTemplatePlaceholder, namespaceID)
}

// TableName returns the table name for the given namespace.
func TableName(nsID string) string {
	return FmtNsID(tableNameTempl, nsID)
}

// fmtSplitIntoTablets replaces the split-into-tablets placeholder in an SQL template string.
// When tablets > 0, it replaces the placeholder with "SPLIT INTO N TABLETS" (YugabyteDB).
// When tablets is 0 or unset, the placeholder is removed, producing standard PostgreSQL-compatible SQL.
func fmtSplitIntoTablets(sqlTemplate string, tablets int) string {
	if tablets > 0 {
		return strings.ReplaceAll(sqlTemplate, splitIntoTabletsPlaceholder,
			" SPLIT INTO "+strconv.Itoa(tablets)+" TABLETS")
	}
	return strings.ReplaceAll(sqlTemplate, splitIntoTabletsPlaceholder, "")
}

// GetTablePreSplitTablets determines the number of tablets to use for table pre-splitting.
// Returns 0 for PostgreSQL or when TablePreSplitTablets is not configured.
// Returns the configured value for YugabyteDB.
func GetTablePreSplitTablets(ctx context.Context, pg *pgxpool.Pool, config *Config) (int, error) {
	if config.TablePreSplitTablets == 0 {
		return 0, nil
	}

	isYugabyte, err := IsYugabyteDB(ctx, pg)
	if err != nil {
		return 0, err
	}
	if !isYugabyte {
		logger.Info("PostgreSQL detected; ignoring table-pre-split-tablets configuration")
		return 0, nil
	}

	logger.Infof("YugabyteDB detected; tables will be pre-split into %d tablets", config.TablePreSplitTablets)
	return config.TablePreSplitTablets, nil
}

// IsYugabyteDB queries the database version string to determine whether the backend is YugabyteDB.
// YugabyteDB's version() output contains "-YB-" (e.g., "PostgreSQL 11.2-YB-2.20.1.0 ..."),
// which distinguishes it from standard PostgreSQL.
func IsYugabyteDB(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var version string
	if err := pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		return false, errors.Wrap(err, "failed to query database version")
	}
	return strings.Contains(version, "-YB-"), nil
}
