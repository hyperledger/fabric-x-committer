#!/usr/bin/env python3

# Copyright IBM Corp All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

start_marker = "<!-- TREE MARKER START -->"
end_marker = "<!-- TREE MARKER END -->"
directory_order = ["ca", "tlsca", "tls", "msp", "orderers", "peers", "users"]


def main():
    if len(sys.argv) < 3:
        sys.exit("Usage: loadgen_artifacts_doc.py <mode> <project_path>")

    mode, project_path = sys.argv[1], Path(sys.argv[2]).resolve()

    tree_output = make_tree(
        bin_file=project_path / "bin" / "loadgen",
        config_file=project_path / "cmd" / "config" / "samples" / "loadgen.yaml",
    )

    doc_file = project_path / "docs" / "loadgen-artifacts.md"
    doc_content = doc_file.read_text()

    start = doc_content.find(start_marker) + len(start_marker)
    end = doc_content.find(end_marker)
    new_content = f"{doc_content[:start]}\n```\n{tree_output}\n```\n{doc_content[end:]}"

    if mode == "check":
        if new_content != doc_content:
            sys.exit("✗ Documentation is out of date. Run 'make generate-sample-tree' to update it.")
    else:
        doc_file.write_text(new_content)
        print(f"✓ Successfully updated {doc_file}")


def make_tree(bin_file, config_file):
    with tempfile.TemporaryDirectory() as tmp_dir_name:
        # Generate artifacts and get the tree.
        subprocess.run(
            [str(bin_file), "make-artifacts", "-c", str(config_file)],
            env={**os.environ, "SC_LOADGEN_LOAD_PROFILE_POLICY_ARTIFACTS_PATH": tmp_dir_name},
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True,
        )
        result = subprocess.run(["tree", "-J", "."], cwd=tmp_dir_name, capture_output=True, text=True, check=True)

    # Format the tree.
    nodes = json.loads(result.stdout)
    # The first node is the root and the second is a summary report.
    root_node = nodes[0]
    return "\n".join(format_tree(root_node))


def format_tree(node: dict):
    """Format tree with collapsed single-child paths."""

    name, sub_nodes = node.get("name", ""), node.get("contents", [])

    # Collapse single-child chains.
    while len(sub_nodes) == 1:
        name = str(os.path.join(name, sub_nodes[0].get("name", "")))
        sub_nodes = sub_nodes[0].get("contents", [])

    yield name

    child_trees = map(format_tree, sorted(sub_nodes, key=node_key))
    for i, (first_line, *more_lines) in enumerate(child_trees):
        is_last_child = i == len(sub_nodes) - 1
        # If there are more children, we add a 3 way connector, then continue the thread until the next children.
        # For the last children, we use a 2 way connector, and only add the indentation.
        children_conn = "├── " if not is_last_child else "└── "
        thread_prefix = "│   " if not is_last_child else "    "
        yield children_conn + first_line
        yield from (thread_prefix + l for l in more_lines)


def node_key(node: dict) -> tuple[int, str]:
    name = node.get("name")
    if name not in directory_order:
        return 1000, name
    return directory_order.index(name), name


if __name__ == "__main__":
    main()
