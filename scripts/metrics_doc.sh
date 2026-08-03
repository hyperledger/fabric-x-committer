#!/bin/bash

# Copyright IBM Corp All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0

# Usage:
#   ./metrics_doc.sh generate  - Generate metrics documentation
#   ./metrics_doc.sh check     - Check if documentation is up to date

repo_root_dir="$(cd "$(dirname "$0")/.." && pwd)"
extract_metrics_script="${repo_root_dir}/scripts/metrics_doc_extract.py"
metrics_doc="${repo_root_dir}/docs/metrics_reference.md"
python_cmd="${python3:-${PYTHON_CMD}}"

# generate_service_doc <service name> <metrics file>... - one section per service, over one or
# more metrics files, for a service whose metrics are split across packages.
generate_service_doc() {
  local service_name="$1"
  shift

  cat <<EOF
## ${service_name} Metrics

The following ${service_name} metrics are exported for consumption by Prometheus.

| Name | Type | Labels | Description |
| ---- | ---- | ------ | ----------- |
EOF
  # Parses the Go metrics files and outputs markdown table rows.
  local metrics_file
  for metrics_file in "$@"; do
    ${python_cmd} "${extract_metrics_script}" "${repo_root_dir}/${metrics_file}"
  done
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
  generate_service_doc "Coordinator" \
    "service/coordinator/metrics.go" \
    "service/coordinator/dependencygraph/metrics.go"
  generate_service_doc "Verifier" "service/verifier/metrics.go"
  generate_service_doc "Validator-Committer" "service/vc/metrics.go"
  generate_service_doc "Query Service" "service/query/metrics.go"
  generate_service_doc "Load Generator" "loadgen/metrics/metrics.go"

  cat <<'EOF'
---
EOF
}

case "$1" in
"check")
  if [ -n "$(diff -u <(generate_doc | markdownfmt) "${metrics_doc}")" ]; then
    echo "The metrics reference documentation is out of date."
    echo "Please run '$0 generate' to update the documentation."
    exit 1
  fi
  ;;

"generate")
  generate_doc | markdownfmt >"${metrics_doc}"
  ;;

*)
  echo "Please specify check or generate"
  exit 1
  ;;
esac
