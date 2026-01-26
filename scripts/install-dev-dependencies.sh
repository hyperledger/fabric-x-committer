#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#
set -e

# Versions for non-Go tools
protoc_bin_version="33.4"
sqlfluff_version="3.4.0"

# Install protoc binary (C++ based, not available via go install)
download_dir=$(mktemp -d -t "sc_dev_depedencies.XXXX")
protoc_zip_download_path="${download_dir}/protoc.zip"

# Determine platform
case "$(uname -s)" in
Linux*) protoc_os="linux" ;;
Darwin*) protoc_os="osx" ;;
*)
  echo "Unsupported OS"
  exit 1
  ;;
esac

case "$(uname -m)" in
x86_64) protoc_arch="x86_64" ;;
aarch64 | arm64) protoc_arch="aarch_64" ;;
*)
  echo "Unsupported architecture"
  exit 1
  ;;
esac

protoc_zip_name="protoc-${protoc_bin_version}-${protoc_os}-${protoc_arch}.zip"
echo "Downloading protoc (${protoc_zip_name}) to ${protoc_zip_download_path}"
curl -L -o "${protoc_zip_download_path}" "https://github.com/protocolbuffers/protobuf/releases/download/v${protoc_bin_version}/${protoc_zip_name}"
echo "Extracting protoc to $HOME/bin"
unzip -jo "${protoc_zip_download_path}" 'bin/*' -d "$HOME/bin"
rm -rf "${download_dir}"

if which apt >/dev/null 2>&1; then
  # Required for "duration" protobuf.
  sudo apt install -y libprotobuf-dev
fi

echo
echo "Installing Go tools from go.mod tool directives"
go install tool

echo
echo "Installing sqlfluff"
${PYTHON_CMD:-python} -m pip install "sqlfluff==${sqlfluff_version}"
