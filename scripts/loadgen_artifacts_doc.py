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
    return "\n".join(format_tree(json.loads(result.stdout)))


def format_tree(node_list: list[dict], prefix="", is_root=True):
    """Format tree with collapsed single-child paths."""

    # Filter the report node (should be the last node of the root list).
    node_list = [n for n in node_list if n.get("type") != "report"]

    for i, child in enumerate(sorted(node_list, key=node_key)):
        name, sub_nodes = child.get("name", ""), child.get("contents", [])
        # Collapse single-child chains.
        while len(sub_nodes) == 1:
            name = str(os.path.join(name, sub_nodes[0]["name"]))
            sub_nodes = sub_nodes[0].get("contents", [])

        if is_root:
            connector = ""
            sub_connector = ""
        elif i == len(node_list) - 1:
            connector = "└── "
            sub_connector = "    "
        else:
            connector = "├── "
            sub_connector = "│   "
        yield prefix + connector + name
        yield from format_tree(sub_nodes, prefix + sub_connector, is_root=False)


def node_key(node: dict) -> tuple[int, str]:
    name = node.get("name")
    if name not in directory_order:
        return 1000, name
    return directory_order.index(name), name


if __name__ == "__main__":
    main()
