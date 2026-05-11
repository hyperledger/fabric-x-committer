#!/usr/bin/env python3

# Copyright IBM Corp All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0

"""
Extracts Prometheus metric definitions from Go source files.

This script parses Go files containing Prometheus metric definitions and outputs
Markdown table rows with: Name, Type, Labels, and Description.

It also follows sub-method calls by parsing their implementations.
"""

import re
import sys
from pathlib import Path

repo_root_dir = Path(__file__).parent

# Find the root directory (contains go.mod)
while repo_root_dir.parent != repo_root_dir:
    if (repo_root_dir / "go.mod").exists():
        break
    repo_root_dir = repo_root_dir.parent

# Pattern: New(Counter|Gauge|Histogram)(Vec)?(prometheus.(Counter|Gauge|Histogram)Opts
# Captures: group(1)=metric type, group(2)=Vec or None, group(3)=opts type
metric_pattern = re.compile(r'New([A-Z][a-z]+)(Vec)?\(prometheus\.([A-Z][a-z]+)Opts')

# Pattern: package.FunctionName(..., monitoring.MetricsParameters
# Captures: group(1)=package, group(2)=method name
params_usage_pattern = re.compile(r'(\w+)\.(\w+)\([^)]+,\s*(?:monitoring\.)?MetricsParameters')


def main():
    """Main entry point."""
    if len(sys.argv) < 2:
        print("Usage: metrics_doc_extract.py <metrics_file>", file=sys.stderr)
        sys.exit(1)

    metrics_file = Path(sys.argv[1])
    if not metrics_file.exists():
        print(f"Warning: {metrics_file} not found", file=sys.stderr)
        return

    print_all_metrics_from_content(metrics_file.read_text())


def print_all_metrics_from_content(content: str):
    """Extract all metrics from Go source content in order of appearance."""
    # Join concatenated strings
    content = re.sub(r'"\s*\+\s*"', '', content)

    # Collect all matches with their positions, sorted by position to maintain order.
    for pos, match_type, *rest in sorted(iter_all_matches(content)):
        params_block = extract_block(content[pos:], "()")
        if not params_block:
            continue

        if match_type == 'metric':
            metric_type, is_vec = rest
            print_single_metric_from_block(metric_type, is_vec, params_block)
            continue

        # match_type == 'method'
        package_name, function_name = rest

        # Determine the source file based on the package name
        source_file = repo_root_dir / "utils" / package_name / "metrics.go"
        if not source_file.exists():
            print(f"Warning: {source_file} not found", file=sys.stderr)
            continue

        # Extract the function body
        func_body = extract_function_body(source_file.read_text(), function_name)
        if not func_body:
            continue

        # Substitute params.Namespace and params.Subsystem in the function body
        namespace = extract_field(params_block, "Namespace")
        func_body = re.sub(r'\bparams\.Namespace\b', f'"{namespace}"', func_body)
        subsystem = extract_field(params_block, "Subsystem")
        func_body = re.sub(r'\bparams\.Subsystem\b', f'"{subsystem}"', func_body)

        # Extract metrics from the substituted function body
        print_all_metrics_from_content(func_body)


def iter_all_matches(content: str):
    # Find all metrics.
    for match in metric_pattern.finditer(content):
        metric_type = match.group(1).lower()  # Counter, Gauge, or Histogram
        is_vec = match.group(2) is not None  # Vec or None
        opts_type = match.group(3).lower()  # Counter, Gauge, or Histogram

        # Verify that the metric type matches the opts type
        assert metric_type == opts_type
        yield match.start(), 'metric', metric_type, is_vec

    # Find all function calls that use monitoring.MetricsParameters{}
    for match in params_usage_pattern.finditer(content):
        package_name = match.group(1)
        function_name = match.group(2)
        yield match.start(), 'method', package_name, function_name


def print_single_metric_from_block(metric_type: str, is_vec: bool, block: str):
    """Print a metric as a Markdown table row."""
    namespace = extract_field(block, "Namespace")
    subsystem = extract_field(block, "Subsystem")
    name = extract_field(block, "Name")
    help_text = extract_field(block, "Help")
    labels = extract_labels(block) if is_vec else ""

    if not name:
        return

    metric_name = namespace
    if subsystem:
        metric_name += f"_{subsystem}"
    metric_name += f"_{name}"

    print(f"| {metric_name} | {metric_type} | {labels} | {help_text} |")


def extract_field(block: str, field: str) -> str:
    """Extract a quoted string value for a given field name."""
    # First try direct quoted string
    match = re.search(rf'{field}\s*:\s*"([^"]*)"', block)
    if match:
        return match.group(1)

    # Try fmt.Sprintf pattern - may span multiple lines
    # Match the format string and all arguments up to the closing paren
    match = re.search(rf'{field}\s*:\s*fmt\.Sprintf\(\s*"([^"]*)",([^)]+)\)', block, re.DOTALL | re.MULTILINE)
    if match:
        format_str = match.group(1)
        args_str = match.group(2)

        # Extract the arguments
        # Remove trailing whitespace, and quotes.
        args = tuple(filter(None, [a.strip().strip('"') for a in args_str.split(",")]))

        # Simple substitution for %s
        return format_str % args

    return ""


def extract_labels(block: str) -> str:
    """Extract label names from a Vec metric definition."""
    match = re.search(r'\[]string\{([^}]*)}', block, re.MULTILINE)
    if not match:
        return ""

    labels_str = match.group(1)
    # Remove quotes and newlines.
    labels_str = re.sub(r'["\n]', '', labels_str)
    # Replace commas with spaces.
    labels_str = re.sub(r',', ' ', labels_str)
    # Remove double spaces.
    labels_str = re.sub(r'\s+', ' ', labels_str)
    return labels_str.strip()


def extract_function_body(content: str, function_name: str) -> str:
    """Extract the body of a function by name."""
    # Pattern: func FunctionName(..., params monitoring.MetricsParameters)
    match = re.search(
        rf'func\s+{function_name}\s*\(([^)]+,)?\s*params\s+(monitoring\.)?MetricsParameters\)',
        content, re.MULTILINE,
    )
    if not match:
        return ""
    return extract_block(content[match.end():], "{}")


def extract_block(content: str, braces="{}") -> str:
    s, e = 0, len(content)
    brace_count = 0
    for m in re.finditer(rf'[{braces}]', content, re.MULTILINE):
        if m.group() == braces[0]:
            if brace_count == 0:
                s = m.end()
            brace_count += 1
        else:  # v == braces[1]
            brace_count -= 1
            if brace_count == 0:
                e = m.start()
                break
    return content[s:e]


if __name__ == "__main__":
    main()
