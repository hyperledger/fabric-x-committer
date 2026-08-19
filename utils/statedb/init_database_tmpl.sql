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

-- 'snapshot hash lease' guards the snapshot hash job so that only one worker
-- computes a given snapshot's hash at a time. The value encodes transaction
-- ID, owner token, and expiry. Owner token fences stale attempts after
-- takeover: only current owner can renew, release, or publish completion.
-- Pre-seeded NULL (no lease held) so the row always exists and plain UPDATE
-- below can be reused, matching 'latest snapshot key'.
INSERT INTO metadata
VALUES ('snapshot hash lease', NULL)
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS tx_status
(
    tx_id  BYTEA NOT NULL PRIMARY KEY,
    status INTEGER,
    height BYTEA NOT NULL
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
    INSERT INTO tx_status (tx_id, status, height)
    VALUES (unnest(_tx_ids),
            unnest(_statuses),
            unnest(_heights));
    RETURN '{}';
EXCEPTION
    WHEN unique_violation THEN
        SELECT array_agg(t.tx_id)
        INTO violating
        FROM tx_status t
        WHERE t.tx_id = ANY (_tx_ids);
        RETURN COALESCE(violating, '{}');
END;
$$ LANGUAGE plpgsql;
