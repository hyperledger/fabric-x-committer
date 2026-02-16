#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#
# Checks that changed Go files (relative to main) are properly formatted
# by goimports and gofumpt. Generated files (*.pb.go) are excluded.
# Also checks YAML linting with yamllint.

set -euo pipefail

GO_CMD="${1:-go}"

EXIT_CODE=0

CHANGED_GO=$(git diff --diff-filter=d --name-only main -- '*.go' | grep -v '.pb.go' || true)

if [[ -z "$CHANGED_GO" ]]; then
  echo "No changed Go files to check."
else
  echo "Running goimports..."
  # shellcheck disable=SC2086
  BAD_IMPORTS=$($GO_CMD tool goimports -l $CHANGED_GO)
  if [[ -n "$BAD_IMPORTS" ]]; then
    echo "goimports check failed:"
    echo "$BAD_IMPORTS"
    EXIT_CODE=1
  fi

  echo "Running gofumpt..."
  # shellcheck disable=SC2086
  BAD_FUMPT=$($GO_CMD tool gofumpt -l $CHANGED_GO)
  if [[ -n "$BAD_FUMPT" ]]; then
    echo "gofumpt check failed:"
    echo "$BAD_FUMPT"
    EXIT_CODE=1
  fi
fi

echo "Running yamllint..."
if ! yamllint .; then
  EXIT_CODE=1
fi

exit $EXIT_CODE
