/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

/*
This SQL file is a template for the querying rows for each namespace.
We use strings.ReplaceAll(sqlTemplate, "${NAMESPACE_ID}", namespaceID) to fill in the the namespace ID.
*/

SELECT
    key,
    value,
    version
FROM ns_${NAMESPACE_ID}
WHERE key = any($1);
