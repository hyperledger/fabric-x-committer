#!/bin/bash

# Copyright IBM Corp All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0

# Usage:
#   ./metrics_doc.sh generate  - Generate metrics documentation
#   ./metrics_doc.sh check     - Check if documentation is up to date

fabricx_dir="$(cd "$(dirname "$0")/.." && pwd)"
scripts_dir="${fabricx_dir}/scripts"
metrics_doc="${fabricx_dir}/docs/metrics_reference.md"

# extract_metrics - Parses a Go metrics file and outputs markdown table rows.
extract_metrics() {
  local filepath="$1"

  if [[ ! -f "$filepath" ]]; then
    echo "Warning: $filepath not found" >&2
    return
  fi

  # Join concatenated strings (" + " on same line or across lines) and extract metrics
  perl -0777 -pe 's/" \+\n\s*"//g; s/" \+ "//g' "$filepath" | awk -f "${scripts_dir}/extract_metrics.awk"
}

generate_service_doc() {
  local service_name="$1"
  local metrics_file="$2"

  cat <<EOF
## ${service_name} Metrics

The following ${service_name} metrics are exported for consumption by Prometheus.

| Name | Type | Labels | Description |
| ---- | ---- | ------ | ----------- |
EOF
  extract_metrics "${fabricx_dir}/${metrics_file}"
  echo ""
}

# generate_doc - Generate the complete metrics documentation
generate_doc() {
  cat <<'EOF'
<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->

# Metrics Reference

EOF

  generate_service_doc "Sidecar" "service/sidecar/metrics.go"
  generate_service_doc "Coordinator" "service/coordinator/metrics.go"
  generate_service_doc "Verifier" "service/verifier/metrics.go"
  generate_service_doc "Validator-Committer" "service/vc/metrics.go"
  generate_service_doc "Query Service" "service/query/metrics.go"

  cat <<'EOF'
---
EOF
}

case "$1" in
"check")
  if [ -n "$(diff -u <(generate_doc) "${metrics_doc}")" ]; then
    echo "The metrics reference documentation is out of date."
    echo "Please run '$0 generate' to update the documentation."
    exit 1
  fi
  ;;

"generate")
  generate_doc >"${metrics_doc}"
  ;;

*)
  echo "Please specify check or generate"
  exit 1
  ;;
esac
