/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v4/pgxpool"
)

const (
	setMetadataPrepSQLStmt      = "UPDATE metadata SET value = $2 WHERE key = $1;"
	getMetadataPrepSQLStmt      = "SELECT value FROM metadata WHERE key = $1;"
	queryTxIDsStatusPrepSQLStmt = "SELECT tx_id, status, height FROM tx_status WHERE tx_id = ANY($1);"

	lastCommittedBlockNumberKey = "last committed block number"

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
)

// NewDatabasePool creates a new pool from a database config.
func NewDatabasePool(ctx context.Context, config *DatabaseConfig) (*pgxpool.Pool, error) {
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

	var pool *pgxpool.Pool
	if retryErr := config.Retry.Execute(ctx, func() error {
		pool, err = pgxpool.ConnectConfig(ctx, poolConfig)
		return errors.Wrap(err, "failed to create a connection pool")
	}); retryErr != nil {
		return nil, retryErr
	}

	logger.Info("DB pool created")
	return pool, nil
}

// TODO: merge this file with database.go.
func (db *database) setupSystemTablesAndNamespaces(ctx context.Context) error {
	logger.Info("Created tx status table, metadata table, and its methods.")
	if execErr := db.retry.ExecuteSQL(ctx, db.pool,
		fmtSplitIntoTablets(dbInitSQLStmt, db.tablePreSplitTablets)); execErr != nil {
		return fmt.Errorf("failed to create system tables and functions: %w", execErr)
	}

	for _, nsID := range systemNamespaces {
		execErr := createNsTables(nsID, db.tablePreSplitTablets, func(q string) error {
			return db.retry.ExecuteSQL(ctx, db.pool, q)
		})
		if execErr != nil {
			return execErr
		}
		logger.Infof("namespace %s: created table and its methods.", nsID)
	}
	return nil
}

func createNsTables(nsID string, tablets int, queryFunc func(q string) error) error {
	query := FmtNsID(createNamespaceSQLStmt, nsID)
	query = fmtSplitIntoTablets(query, tablets)
	if err := queryFunc(query); err != nil {
		return errors.Wrapf(err, "failed to create table and functions for namespace [%s] with query [%s]",
			nsID, query)
	}
	return nil
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
