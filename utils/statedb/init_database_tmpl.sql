/*
 * Copyright IBM Corp. All Rights Reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

-- This SQL is the flow to initiate the DB for the committer.

CREATE TABLE IF NOT EXISTS metadata
(
    key   BYTEA NOT NULL PRIMARY KEY,
    value BYTEA
)${SPLIT_INTO_TABLETS};

INSERT INTO metadata
VALUES ('last committed block number', NULL)
ON CONFLICT DO NOTHING;

INSERT INTO metadata
VALUES ('latest snapshot key', NULL)
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS tx_status
(
    tx_id  BYTEA NOT NULL PRIMARY KEY,
    status INTEGER,
    height BYTEA NOT NULL
)${SPLIT_INTO_TABLETS};

CREATE TABLE IF NOT EXISTS migrated_tx_ids
(
    tx_id BYTEA NOT NULL PRIMARY KEY
)${SPLIT_INTO_TABLETS};

CREATE TABLE IF NOT EXISTS migration_record
(
    singleton_id            SMALLINT NOT NULL PRIMARY KEY DEFAULT 1 CHECK (singleton_id = 1),
    migration_id            BYTEA NOT NULL UNIQUE,
    source_channel          TEXT NOT NULL,
    source_block_number     BIGINT NOT NULL CHECK (source_block_number >= 0),
    source_snapshot_hash    BYTEA NOT NULL,
    target_anchor           BIGINT NOT NULL CHECK (target_anchor >= 0),
    target_config_hash      BYTEA NOT NULL,
    namespace_map_hash      BYTEA NOT NULL,
    target_policy_hash      BYTEA NOT NULL,
    public_state_count      BIGINT NOT NULL CHECK (public_state_count >= 0),
    public_state_hash       BYTEA NOT NULL,
    transaction_id_count   BIGINT NOT NULL CHECK (transaction_id_count >= 0),
    transaction_id_hash    BYTEA NOT NULL,
    status                  TEXT NOT NULL CHECK (status IN ('VERIFIED', 'ACTIVE'))
)${SPLIT_INTO_TABLETS};

CREATE OR REPLACE FUNCTION insert_tx_status(
    IN _tx_ids BYTEA[],
    IN _statuses INTEGER[],
    IN _heights BYTEA[]
) RETURNS BYTEA[]
AS
$$
DECLARE
    violating BYTEA[];
BEGIN
    SELECT array_agg(v.tx_id ORDER BY v.tx_id)
    INTO violating
    FROM (
        SELECT i.tx_id
        FROM unnest(_tx_ids) AS i(tx_id)
        GROUP BY i.tx_id
        HAVING count(*) > 1
        UNION
        SELECT t.tx_id FROM tx_status t WHERE t.tx_id = ANY (_tx_ids)
        UNION
        SELECT m.tx_id FROM migrated_tx_ids m WHERE m.tx_id = ANY (_tx_ids)
    ) AS v;

    IF cardinality(violating) > 0 THEN
        RETURN violating;
    END IF;

    INSERT INTO tx_status (tx_id, status, height)
    VALUES (unnest(_tx_ids),
            unnest(_statuses),
            unnest(_heights));
    RETURN '{}';
EXCEPTION
    WHEN unique_violation THEN
        SELECT array_agg(t.tx_id)
        INTO violating
        FROM (
            SELECT t.tx_id FROM tx_status t WHERE t.tx_id = ANY (_tx_ids)
            UNION
            SELECT m.tx_id FROM migrated_tx_ids m WHERE m.tx_id = ANY (_tx_ids)
        ) AS t;
        RETURN COALESCE(violating, '{}');
END;
$$ LANGUAGE plpgsql;
