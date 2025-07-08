# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#
#########################
# Makefile Targets Summary
#########################

# test: Builds binaries and runs both unit and integration tests
# clean: Removes all binaries
# proto: Generates all committer's API protobufs
# build: Builds all binaries
# build-arch: Builds all binaries for linux/(<cur-arch> amd64 arm64 s390x)
# build-docker: Builds all binaries in a docker container
# docker-builder-run: Executes a command from within a golang docker image.
# docker-runner-image: Builds the committer docker image containing all binaries
# lint: Runs golangci-lint

#########################
# Constants
#########################

go_cmd          ?= go
version         := 0.0.2
project_dir     := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
output_dir      ?= $(project_dir)/bin
arch_output_dir ?= $(project_dir)/archbin
cache_dir       ?= $(shell $(go_cmd) env GOCACHE)
mod_cache_dir   ?= $(shell $(go_cmd) env GOMODCACHE)
go_version      ?= 1.24.3
golang_image    ?= golang:$(go_version)-bookworm

dockerfile_base_dir       ?= $(project_dir)/docker/images
dockerfile_test_node_dir  ?= $(dockerfile_base_dir)/test_node
dockerfile_release_dir    ?= $(dockerfile_base_dir)/release

# Set this parameter when running docker-builder-run
# E.g., make docker-builder-run cmd="make build-local"
cmd            ?=

# Set the 'docker_cmd' variable directly (e.g., docker_cmd="podman") to override automatic detection.
# If the 'docker_cmd' variable is not set, the script will find and use either Docker or Podman.
# An error will occur if neither container engine is installed.
docker_cmd ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || \
							echo podman || { echo "Error: Neither Docker nor Podman is installed." >&2; exit 1; })
image_namespace=icr.io/cbdc

# Set these parameters to compile to a specific os/arch
# E.g., make build-local os=linux arch=amd64
os             ?= $(shell $(go_cmd) env GOOS)
arch           ?= $(shell $(go_cmd) env GOARCH)
multiplatform  ?= false
env            ?= env GOOS=$(os) GOARCH=$(arch)
build_flags    ?= -buildvcs=false -o
go_build       ?= $(env) $(go_cmd) build $(build_flags)
go_test        ?= $(go_cmd) test -json -v -timeout 30m

arch_output_dir_rel = $(arch_output_dir:${project_dir}/%=%)

PYTHON_CMD ?= python

# Set additional parameter to build the test-node for different platforms and push
# E.g., make multiplatform=true docker_push=true build-test-node-image
docker_build_flags=--quiet
ifeq "$(multiplatform)" "true"
	docker_build_flags+=--platform linux/amd64,linux/arm64
endif

docker_push ?= false
docker_push_arg = 
ifeq "$(docker_push)" "true"
	docker_push_arg=--push
endif

MAKEFLAGS += --jobs=16

#########################
# Tests
#########################

ROOT_PKG_REGEXP = github.com/hyperledger/fabric-x-committer
CORE_DB_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/service/(vc|query)
REQUIRES_DB_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/(service/coordinator|loadgen|cmd)
HEAVY_PACKAGES_REGEXP = ${ROOT_PKG_REGEXP}/(docker|integration)

NON_HEAVY_PACKAGES=$(shell $(go_cmd) list ./... | grep -vE "$(HEAVY_PACKAGES_REGEXP)")
COR_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -E "$(CORE_DB_PACKAGES_REGEXP)")
REQUIRES_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -E "$(REQUIRES_DB_PACKAGES_REGEXP)")
NO_DB_PACKAGES=$(shell $(go_cmd) list ./... | grep -vE "$(CORE_DB_PACKAGES_REGEXP)|$(REQUIRES_DB_PACKAGES_REGEXP)|$(HEAVY_PACKAGES_REGEXP)")

GO_TEST_FMT_FLAGS := -hide empty-packages


# Excludes integration and container tests.
# Use `test-integration`, `test-integration-db-resiliency`, and `test-container`.
test: build
	@$(go_test) ${NON_HEAVY_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

# Test a specific package.
test-package-%: build
	@$(go_test) ./$*/... | gotestfmt ${GO_TEST_FMT_FLAGS}

# Integration tests excluding DB resiliency tests.
# Use `test-integration-db-resiliency`.
test-integration: build
	@$(go_test) ./integration/... -skip "DBResiliency.*" | gotestfmt ${GO_TEST_FMT_FLAGS}

# DB resiliency integration tests.
test-integration-db-resiliency: build
	@$(go_test) ./integration/... -run "DBResiliency.*" | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests the all-in-one docker image.
test-container: build-test-node-image
	$(go_cmd) test -v -timeout 30m ./docker/...

# Tests for components that directly talk to the DB, where different DBs might affect behaviour.
test-core-db: build
	@$(go_test)  ${COR_DB_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests for components that depend on the DB layer, but are agnostic to the specific DB used.
test-requires-db: build
	@$(go_test) ${REQUIRES_DB_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests the ASN.1 marshalling using fuzz testing.
test-fuzz: build
	@$(go_test) -run="^$$" -fuzz=".*" -fuzztime=5m ./utils/signature | gotestfmt ${GO_TEST_FMT_FLAGS}

# Tests that require no DB at all, e.g., pure logic, utilities
test-no-db: build
	@$(go_test) ${NO_DB_PACKAGES} | gotestfmt ${GO_TEST_FMT_FLAGS}

test-cover: build
	$(go_cmd) test -v -coverprofile=coverage.profile ./...

test-cover-%: build
	$(go_cmd) test -v -coverprofile=$*.coverage.profile "./$*/..."

cover-report: FORCE
	$(go_cmd) tool cover -html=coverage.profile

cover-report-%: FORCE
	$(go_cmd) tool cover -html=$*.coverage.profile

clean: FORCE
	@rm -rf $(output_dir)
	@rm -rf $(arch_output_dir)

kill-test-docker: FORCE
	$(docker_cmd) ps -aq -f name=sc_yugabyte_unit_tests | xargs $(docker_cmd) rm -f
	$(docker_cmd) ps -aq -f name=sc_postgres_unit_tests | xargs $(docker_cmd) rm -f

#########################
# Benchmarks
#########################

# Run a load generation benchmarks with added TX/sec column.
bench-loadgen: FORCE
	$(go_cmd) test ./loadgen/... -bench "Benchmark.*" -run="^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run dependency detector benchmarks with added op/sec column.
bench-dep: FORCE
	$(go_cmd) test ./service/coordinator/dependencygraph/... -bench "BenchmarkDependencyGraph.*" -run="^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run dependency detector benchmarks with added op/sec column.
bench-preparer: FORCE
	$(go_cmd) test ./service/vc/... -bench "BenchmarkPrepare.*" -run "^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run signature benchmarks with added op/sec column.
bench-sign: FORCE
	$(go_cmd) test ./utils/signature/... -bench ".*" -run "^$$" | awk -f scripts/bench-tx-per-sec.awk

# Run verifier benchmarks with added op/sec column.
bench-verify: FORCE
	$(go_cmd) test ./service/verifier/... -bench "BenchmarkVerifyForm.*" -run "^$$" | awk -f scripts/bench-tx-per-sec.awk

#########################
# Generate protos
#########################

PROTO_TARGETS ?= $(shell find ./api \
	 -name '*.proto' -print0 | \
	 xargs -0 -n 1 dirname | xargs -n 1 basename | \
	 sort -u | sed -e "s/^proto/proto-/" \
)

proto: $(PROTO_TARGETS)

proto-%: FORCE
	@echo "Compiling: $*"
	@protoc --proto_path="${PWD}" \
          --go-grpc_out=. --go-grpc_opt=paths=source_relative \
          --go_out=paths=source_relative:. ${PWD}/api/proto$*/*.proto

#########################
# Binaries
#########################

$(output_dir):
	mkdir -p "$(output_dir)"

$(cache_dir) $(mod_cache_dir):
	# Use the host local gocache and gomodcache folder to avoid rebuilding and re-downloading every time
	mkdir -p "$(cache_dir)" "$(mod_cache_dir)"

BUILD_TARGETS=build-cli-committer build-cli-loadgen build-cli-mock

build: $(output_dir) $(BUILD_TARGETS)

build-arch: build-arch-linux-$(arch) build-arch-linux-amd64 build-arch-linux-arm64 build-arch-linux-s390x

build-arch-%: FORCE
	@CGO_ENABLED=0 make \
		os=$(word 1, $(subst -, ,$*)) \
		arch=$(word 2, $(subst -, ,$*)) \
		output_dir=$(arch_output_dir)/$* \
		build_flags="-ldflags '-w -s' $(build_flags)" \
		build

build-cli-%: FORCE $(output_dir)
	$(go_build) "$(output_dir)/$*" "./cmd/$*"

build-docker: FORCE $(cache_dir) $(mod_cache_dir)
	$(docker_cmd) run --rm -it \
	  --mount "type=bind,source=$(project_dir),target=$(project_dir)" \
	  --mount "type=bind,source=$(cache_dir),target=$(cache_dir)" \
	  --mount "type=bind,source=$(mod_cache_dir),target=$(mod_cache_dir)" \
	  --workdir $(project_dir) \
	  --env GOCACHE="$(cache_dir)" \
	  --env GOMODCACHE="$(mod_cache_dir)" \
	  $(golang_image) \
    make build output_dir=$(output_dir) env="$(env)"
	scripts/amend-permissions.sh "$(cache_dir)" "$(mod_cache_dir)"

build-test-node-image: build-arch build-test-genesis-block
	${docker_cmd} build $(docker_build_flags) \
		-f $(dockerfile_test_node_dir)/Dockerfile \
		-t ${image_namespace}/committer-test-node:${version} \
		--build-arg ARCHBIN_PATH=${arch_output_dir_rel} \
		. $(docker_push_arg)

build-release-image: build-arch
	./scripts/build-release-image.sh \
		$(docker_cmd) $(version) $(image_namespace) $(dockerfile_release_dir) $(multiplatform) $(arch_output_dir_rel)

build-test-genesis-block: $(output_dir) build-cli-loadgen
	@# We load the env from the Dockerfile to use them to generate the config block.
	env -v $(shell grep '^ENV' $(dockerfile_test_node_dir)/Dockerfile | cut -d' ' -f2- | xargs) \
		bin/loadgen make-genesis-block \
		-c "$(project_dir)/cmd/config/samples/loadgen.yaml" \
		>"$(output_dir)/sc-genesis-block.proto.bin"

#########################
# Linter
#########################

lint: FORCE
	@echo "Running Go Linters..."
	golangci-lint run --color=always --new-from-rev=main --timeout=4m
	@echo "Running SQL Linters..."
	git ls-files '*.sql' | sort -u | ${PYTHON_CMD} -m sqlfluff lint --dialect postgres
	@echo "Running License Header Linters..."
	scripts/license-lint.sh

# This rule can be used to find and fix lint issues for specific package.
full-lint-%: FORCE
	golangci-lint run --color=always --timeout=4m ./$*/...

full-lint: FORCE
	golangci-lint run --color=always --timeout=4m ./...

# https://www.gnu.org/software/make/manual/html_node/Force-Targets.html
# If a rule has no prerequisites or recipe, and the target of the rule is a nonexistent file,
# then make imagines this target to have been updated whenever its rule is run.
# This implies that all targets depending on this one will always have their recipe run.
FORCE:
