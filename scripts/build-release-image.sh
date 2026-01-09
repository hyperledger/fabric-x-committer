#!/usr/bin/env bash
#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

set -euxo pipefail

docker_cmd=$1
version=$2
namespace=$3
dockerfile_release_dir=$4
multiplatform=$5
arch_bin_dir=$6

function build_image() {
  local image_name=$1
  local bin=$2
  local service_ports=$3
  local manifest_name=${namespace}/fabric-x-${image_name}:${version}
  local cmd=(
    "${docker_cmd}" build
    -f "${dockerfile_release_dir}/Dockerfile"
    --build-arg BIN="${bin}"
    --build-arg PORTS="${service_ports}"
    --build-arg ARCHBIN_PATH="${arch_bin_dir}"
    --build-arg VERSION="${version}"
    --build-arg CREATED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    --build-arg REVISION="$(git rev-parse HEAD)"
  )

  if [ "${multiplatform}" = true ]; then
    # Multi-platform build
    ${docker_cmd} images "${manifest_name}" -q -a | xargs "${docker_cmd}" rmi
    ${docker_cmd} manifest create "${manifest_name}"
    "${cmd[@]}" --jobs=4 --platform linux/amd64,linux/arm64,linux/s390x --manifest "${manifest_name}" .
  else
    # Current platform build
    "${cmd[@]}" -t "${manifest_name}" .
  fi
}

# to build the sc binaries for z, make sure you use the
# following make command before building the container images
#
# make GOOS=linux GOARCH=s390x build

# build container images
build_image committer committer "4001 2114 9001 2119 5001 2115 6001 2116 7001 2117"
build_image loadgen loadgen "8001 2118"
${docker_cmd} images | grep -E "committer|loadgen"
