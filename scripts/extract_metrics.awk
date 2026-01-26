# Copyright IBM Corp All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0

# extract_metrics.awk - Extracts Prometheus metric definitions from Go source files.
# This script parses Go files containing Prometheus metric definitions and outputs
# markdown table rows with: Name, Type, Labels, and Description.
#
# Usage:
#   perl -0777 -pe 's/" \+\n\s*"//g; s/" \+ "//g' metrics.go | awk -f extract_metrics.awk
#
# The perl command joins Go string concatenations (both multi-line and single-line).
#
# The script expects Prometheus metrics defined using the prometheus client_golang
# library with the following patterns:
#
# Single metric (Counter, Gauge, Histogram):
#
#   p.NewCounter(prometheus.CounterOpts{
#       Namespace: "service_name",
#       Subsystem: "component",
#       Name:      "metric_name",
#       Help:      "Description of the metric.",
#   })
#
# Vector metric with labels (CounterVec, GaugeVec, HistogramVec):
#
#   p.NewCounterVec(prometheus.CounterOpts{
#       Namespace: "service_name",
#       Subsystem: "component",
#       Name:      "metric_name",
#       Help:      "Description of the metric.",
#   }, []string{"label1", "label2"})
#
# Notes:
#   - Only Subsystem is optional
#   - String concatenations using Go's " + " are joined by the perl preprocessor
#   - The metric block must end with })

BEGIN {
    NOT_VEC = 0
    IS_VEC  = 1
}

/NewCounter\(prometheus\.CounterOpts/        { print_metric("counter", NOT_VEC) }
/NewCounterVec\(prometheus\.CounterOpts/     { print_metric("counter", IS_VEC) }
/NewGauge\(prometheus\.GaugeOpts/            { print_metric("gauge", NOT_VEC) }
/NewGaugeVec\(prometheus\.GaugeOpts/         { print_metric("gauge", IS_VEC) }
/NewHistogram\(prometheus\.HistogramOpts/    { print_metric("histogram", NOT_VEC) }
/NewHistogramVec\(prometheus\.HistogramOpts/ { print_metric("histogram", IS_VEC) }

# ------------------------------------------------------------------------------
# Functions
# ------------------------------------------------------------------------------

function print_metric(type, is_vec) {
    # Accumulate lines until we find the closing })
    block = $0
    while (block !~ /}\)/) {
        if ((getline line) <= 0) break
        block = block "\n" line
    }

    namespace = extract_field(block, "Namespace:")
    subsystem = extract_field(block, "Subsystem:")
    name      = extract_field(block, "Name:")
    help      = extract_field(block, "Help:")
    labels    = is_vec ? extract_labels(block) : ""

    metric_name = namespace
    if (subsystem != "") { # subsystem is optional.
        metric_name = metric_name "_" subsystem
    }
    metric_name = metric_name "_" name

    if (name != "") {
        printf "| %s | %s | %s | %s |\n", metric_name, type, labels, help
    }
}

# extract_field - Extract a quoted string value for a given field name
function extract_field(block, field) {
    if (block !~ field) return ""
    value = block
    sub(".*" field " *\"", "", value)  # Remove everything up to opening quote
    sub("\".*", "", value)             # Remove closing quote and rest
    return value
}

# extract_labels - Extract label names from a Vec metric definition
function extract_labels(block) {
    if (block !~ /\[\]string\{/) return ""
    value = block
    sub(/.*\[\]string\{/, "", value)  # Remove everything up to []string{
    sub(/\}.*/, "", value)            # Remove closing brace and rest
    gsub(/\n/, "", value)             # Remove newlines (for multi-line labels)
    gsub(/"/, "", value)              # Remove all double quotes
    gsub(/,/, " ", value)             # Replace commas with space
    gsub(/  +/, " ", value)           # Collapse multiple spaces into one
    gsub(/^ +| +$/, "", value)        # Trim leading and trailing spaces
    return value
}
