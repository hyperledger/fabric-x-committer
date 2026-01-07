#!/bin/bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#
set -e

# Versions
protoc_bin_version="29.3"
protoc_gen_go_version="v1.33"
protoc_gen_go_grpc_version="v1.3"
goimports_version="v0.33.0"
gotestfmt_version="v2.5.0"
golang_ci_version="v2.7.2"
sqlfluff_version="3.4.0"

download_dir=$(mktemp -d -t "sc_dev_depedencies.XXXX")
protoc_zip_download_path="${download_dir}/protoc.zip"
echo "Downloading protoc to ${protoc_zip_download_path}"
curl -L -o "${protoc_zip_download_path}" "https://github.com/protocolbuffers/protobuf/releases/download/v${protoc_bin_version}/protoc-${protoc_bin_version}-linux-x86_64.zip"
echo "Extracting protoc to $HOME/bin"
unzip -jo "${protoc_zip_download_path}" 'bin/*' -d "$HOME/bin"
rm -rf "${download_dir}"

if which apt >/dev/null 2>&1; then
  # Required for "duration" protobuf.
  sudo apt install -y libprotobuf-dev
fi

echo
echo "Installing protoc-gen-go"
go install "google.golang.org/protobuf/cmd/protoc-gen-go@${protoc_gen_go_version}"
echo
echo "Installing protoc-gen-go-grpc"
go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${protoc_gen_go_grpc_version}"
echo
echo "Installing goimports"
go install "golang.org/x/tools/cmd/goimports@${goimports_version}"
echo
echo "Installing gotestfmt"
go install "github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@${gotestfmt_version}"

echo
echo "Installing golangci-lint"
curl -sSfL "https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh" | sh -s -- -b "$(go env GOPATH)/bin" "${golang_ci_version}"

echo
echo "Installing sqlfluff"
${PYTHON_CMD:-python} -m pip install "sqlfluff==${sqlfluff_version}"
