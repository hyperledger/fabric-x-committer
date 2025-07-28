/*
 * Copyright IBM Corp. All Rights Reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

/*
This SQL file is a template for creating a new namespace.
We use strings.ReplaceAll(sqlTemplate, "${NAMESPACE_ID}", namespaceID) to fill in the the namespace ID.

For each new namespace ID, we create a table ns_<namespace-ID> and three methods:
- insert_ns_<namespace-ID>: Inserted new keys with version 0. Fails for keys that already exists.
- update_ns_<namespace-ID>: Update existing keys. Fails for keys that doesn't exist.
- validate_reads_ns_<namespace-ID>: Validate the key's version.
*/

CREATE TABLE IF NOT EXISTS ns_${NAMESPACE_ID}
(
    key     BYTEA                    NOT NULL PRIMARY KEY,
    value   BYTEA  DEFAULT NULL,
    version BIGINT DEFAULT 0::BIGINT NOT NULL CHECK (version >= 0)
);

CREATE OR REPLACE FUNCTION insert_ns_${NAMESPACE_ID}(
    IN _keys BYTEA[],
    IN _values BYTEA[]
) RETURNS BYTEA[]
AS
$$
DECLARE
    violating BYTEA[];
BEGIN
    INSERT INTO ns_${NAMESPACE_ID} (key, value)
    SELECT k, v
    FROM unnest(_keys, _values) AS t(k, v);
    RETURN '{}';
EXCEPTION
    WHEN unique_violation THEN
        SELECT array_agg(t_existing.key)
        INTO violating
        FROM ns_${NAMESPACE_ID} t_existing
        WHERE t_existing.key = ANY (_keys);
        RETURN COALESCE(violating, '{}');
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION update_ns_${NAMESPACE_ID}(
    IN _keys BYTEA[],
    IN _values BYTEA[],
    IN _versions BIGINT[]
)
    RETURNS VOID
AS
$$
BEGIN
    UPDATE ns_${NAMESPACE_ID}
    SET value   = t.value,
        version = t.version
    FROM (SELECT *
          FROM unnest(_keys, _values, _versions) AS t(key, value, version)) AS t
    WHERE ns_${NAMESPACE_ID}.key = t.key;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION validate_reads_ns_${NAMESPACE_ID}(
    keys BYTEA[],
    versions BIGINT[]
) RETURNS INTEGER[]
AS
$$
DECLARE
    bad_indices INTEGER[];
BEGIN
    SELECT array_agg(expected.idx)
    INTO bad_indices
    FROM unnest(keys, versions) WITH ORDINALITY AS expected(key, version, idx)
             LEFT JOIN
         ns_${NAMESPACE_ID} actual ON actual.key = expected.key
    WHERE -- Followed are mismatch detected
       -- The key does not exist in the committed state but expected version is not null
        (actual.key IS NULL AND expected.version IS NOT NULL)
       OR -- The key exists in the committed state but expected version is null
        (actual.key is NOT NULL AND expected.version IS NULL)
       OR -- The committed version of a key is different from the expected version
        (actual.version IS DISTINCT FROM expected.version);

    RETURN COALESCE(bad_indices, '{}');
END;
$$ LANGUAGE plpgsql;
