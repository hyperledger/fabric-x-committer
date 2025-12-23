#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

###########################################
# Stage 1: Production runtime image
###########################################
FROM postgres:16.9-alpine3.21 AS node

ARG ARCHBIN_PATH
ARG TARGETOS
ARG TARGETARCH
ARG VERSION

ENV CONFIGS_PATH=/root/config
ENV BINS_PATH=/root/bin
ENV PATH="$BINS_PATH:$PATH"

# Set endpoints to be local.
ENV SC_COORDINATOR_VERIFIER_ENDPOINTS=localhost:5001
ENV SC_COORDINATOR_VALIDATOR_COMMITTER_ENDPOINTS=localhost:6001
ENV SC_SIDECAR_ORDERER_CONNECTION_ENDPOINTS=localhost:7050
ENV SC_SIDECAR_COMMITTER_ENDPOINT=localhost:9001
ENV SC_QUERY_DATABASE_ENDPOINTS=localhost:5433
ENV SC_VC_DATABASE_ENDPOINTS=localhost:5433
ENV SC_LOADGEN_ORDERER_CLIENT_ORDERER_CONNECTION_ENDPOINTS=localhost:7050
ENV SC_LOADGEN_LOAD_PROFILE_TRANSACTION_POLICY_ORDERER_ENDPOINTS="id=0,msp-id=org,broadcast,deliver,localhost:7050"
ENV SC_LOADGEN_ORDERER_CLIENT_SIDECAR_CLIENT_ENDPOINT=localhost:4001

# Set TLS mode to none-tls.
ENV SC_COORDINATOR_SERVER_TLS_MODE="none"
ENV SC_COORDINATOR_VERIFIER_TLS_MODE="none"
ENV SC_COORDINATOR_VALIDATOR_COMMITTER_TLS_MODE="none"
ENV SC_LOADGEN_SERVER_TLS_MODE="none"
ENV SC_LOADGEN_ORDERER_CLIENT_SIDECAR_CLIENT_TLS_MODE="none"
ENV SC_QUERY_SERVER_TLS_MODE="none"
ENV SC_SIDECAR_SERVER_TLS_MODE="none"
ENV SC_SIDECAR_COMMITTER_TLS_MODE="none"
ENV SC_VC_SERVER_TLS_MODE="none"
ENV SC_VERIFIER_SERVER_TLS_MODE="none"
ENV SC_ORDERER_SERVER_TLS_MODE="none"
ENV SC_SIDECAR_ORDERER_CONNECTION_TLS_MODE="none"
ENV SC_LOADGEN_ORDERER_CLIENT_ORDERER_CONNECTION_TLS_MODE="none"

# Disable TLS usage for db.
ENV SC_VC_DATABASE_TLS_MODE="none"
ENV SC_QUERY_DATABASE_TLS_MODE="none"

COPY ${ARCHBIN_PATH}/${TARGETOS}-${TARGETARCH}/* ${BINS_PATH}/
COPY ./docker/images/test_node/run ${BINS_PATH}/
COPY ./cmd/config/samples $CONFIGS_PATH
COPY ./bin/sc-genesis-block.proto.bin $CONFIGS_PATH/
RUN chmod a+x ${BINS_PATH}/*

# Expose ports
EXPOSE 7050 4001 2114 7001 2117 2110

# OCI metadata labels
LABEL name="fabric-x-committer-test-node" \
    maintainer="IBM Research ZRL Decentralized Trust Group" \
    version="${VERSION}" \
    description="Preconfigured Hyperledger Fabric-X Committer Node" \
    license="Apache-2.0" \
    vendor="IBM"

# Default CMD
CMD ["run"]
