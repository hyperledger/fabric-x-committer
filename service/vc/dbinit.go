/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/yugabyte/pgx/v4/pgxpool"

	"github.com/hyperledger/fabric-x-committer/api/types"
)

const (
	// system tables and function names.
	txStatusTableName      = "tx_status"
	metadataTableName      = "metadata"
	insertTxStatusFuncName = "insert_tx_status"

	// namespace table and function names prefix.
	nsTableNamePrefix               = "ns_"
	validateReadsOnNsFuncNamePrefix = "validate_reads_"
	updateNsStatesFuncNamePrefix    = "update_"
	insertNsStatesFuncNamePrefix    = "insert_"

	setMetadataPrepSQLStmt      = "UPDATE " + metadataTableName + " SET value = $2 WHERE key = $1;"
	getMetadataPrepSQLStmt      = "SELECT value FROM " + metadataTableName + " WHERE key = $1;"
	queryTxIDsStatusPrepSQLStmt = "SELECT tx_id, status, height FROM " + txStatusTableName + " WHERE tx_id = ANY($1);"

	lastCommittedBlockNumberKey = "last committed block number"

	// nsIDTemplatePlaceholder is used as a template placeholder for SQL queries.
	nsIDTemplatePlaceholder = "${NAMESPACE_ID}"
)

var (
	//go:embed init_database.sql
	dbInitSQLStmt string
	//go:embed create_namespace.sql
	createNamespaceSQLStmt string

	systemNamespaces = []string{types.MetaNamespaceID, types.ConfigNamespaceID}
)

// NewDatabasePool creates a new pool from a database config.
func NewDatabasePool(ctx context.Context, config *DatabaseConfig) (*pgxpool.Pool, error) {
	logger.Infof("DB source: %s", config.DataSourceName())
	poolConfig, err := pgxpool.ParseConfig(config.DataSourceName())
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
	if execErr := db.retry.ExecuteSQL(ctx, db.pool, dbInitSQLStmt); execErr != nil {
		return fmt.Errorf("failed to create system tables and functions: %w", execErr)
	}

	for _, nsID := range systemNamespaces {
		execErr := createNsTables(nsID, func(q string) error {
			return db.retry.ExecuteSQL(ctx, db.pool, q)
		})
		if execErr != nil {
			return execErr
		}
		logger.Infof("namespace %s: created table and its methods.", nsID)
	}
	return nil
}

func createNsTables(nsID string, queryFunc func(q string) error) error {
	query := FmtNsID(createNamespaceSQLStmt, nsID)
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
