#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#
REQUIRED_HEADER="SPDX-License-Identifier: Apache-2.0"

# - JSON does not support comments.
# - `goheader` linter already covers the `.go` files.
# - `go.sum` is automatically generated from the `go.mod` file.
IGNORE_REGEXP="(.*\.(json|go)|go.sum|LICENSE|AGENTS.md|\.bob/.*|\.claude/.*)$"

# Symlinks may point outside the repo (e.g. generated files or vendored assets) and their
# targets are not required to carry a license header. `[ -L "$f" ]` returns true for symlinks,
# so the negated test `|| echo "$f"` only forwards regular files to the header check.
missing=$(git ls-files | sort -u | grep -vE "${IGNORE_REGEXP}" | \
  while read -r f; do [ -L "$f" ] || echo "$f"; done | \
  xargs grep -L "${REQUIRED_HEADER}")

if [[ -z "$missing" ]]; then
  exit 0
fi

echo "Files without license headers:"
echo "------------------------------"
echo "$missing"
echo "--- FAIL"
exit 1
