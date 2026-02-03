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
ENV MATERIAL_PATH=/root/material
ENV BINS_PATH=/root/bin
ENV PATH="$BINS_PATH:$PATH"

# Set endpoints to be local.
ENV SC_COORDINATOR_VERIFIER_ENDPOINTS=localhost:5001
ENV SC_COORDINATOR_VALIDATOR_COMMITTER_ENDPOINTS=localhost:6001
ENV SC_QUERY_DATABASE_ENDPOINTS=localhost:5433
ENV SC_VC_DATABASE_ENDPOINTS=localhost:5433
ENV SC_SIDECAR_COMMITTER_ENDPOINT=localhost:9001
ENV SC_SIDECAR_ORDERER_ORGANIZATIONS_ORG0_ENDPOINTS="id=0,broadcast,deliver,localhost:7050"
ENV SC_LOADGEN_ORDERER_CLIENT_ORDERER_ORGANIZATIONS_ORG0_ENDPOINTS="id=0,broadcast,deliver,localhost:7050"
# This variable defines the Orderer endpoints in the generated genesis block.
ENV SC_LOADGEN_LOAD_PROFILE_POLICY_ORDERER_ENDPOINTS="id=0,msp-id=org,broadcast,deliver,localhost:7050"
ENV SC_LOADGEN_ORDERER_CLIENT_SIDECAR_CLIENT_ENDPOINT=localhost:4001

# Disable TLS usage for db.
ENV SC_VC_DATABASE_TLS_MODE="none"
ENV SC_QUERY_DATABASE_TLS_MODE="none"

COPY ${ARCHBIN_PATH}/${TARGETOS}-${TARGETARCH}/* ${BINS_PATH}/
COPY ./docker/images/test_node/run ${BINS_PATH}/
COPY ./cmd/config/samples $CONFIGS_PATH
RUN chmod a+x ${BINS_PATH}/*

# Generate the genesis block and the crypto material for the test-container.
# This includes private keys, public keys, TLS keys, etc.
RUN ${BINS_PATH}/loadgen make-material -c "${CONFIGS_PATH}/loadgen.yaml"

# Expose ports
EXPOSE 7050 4001 2114 7001 2117 2110

# OCI metadata labels
LABEL org.opencontainers.image.created="${CREATED}" \
    org.opencontainers.image.description="Hyperledger Fabric-X Committer Test Node." \
    org.opencontainers.image.licenses="Apache-2.0" \
    org.opencontainers.image.ref.name="postgres" \
    org.opencontainers.image.revision="${REVISION}" \
    org.opencontainers.image.source="https://github.com/hyperledger/fabric-x-committer" \
    org.opencontainers.image.title="fabric-x-committer-test-node" \
    org.opencontainers.image.url="https://github.com/hyperledger/fabric-x-committer" \
    org.opencontainers.image.version="${VERSION}"

# Default CMD
CMD ["run"]
