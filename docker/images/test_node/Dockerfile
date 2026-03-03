#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

###########################################
# Stage 1: Production runtime image
###########################################
FROM postgres:16.9-alpine3.21 AS node

ARG SRC_BIN_PATH
ARG TARGETOS
ARG TARGETARCH
ARG VERSION

ENV CONFIGS_PATH=/root/config
ENV ARTIFACTS_PATH=/root/artifacts
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

COPY ${SRC_BIN_PATH}/${TARGETOS}-${TARGETARCH}/* ${BINS_PATH}/
COPY ./docker/images/test_node/run ${BINS_PATH}/
COPY ./cmd/config/samples $CONFIGS_PATH
RUN chmod a+x ${BINS_PATH}/*

# Generate the genesis block and the crypto artifacts for the test-container.
# This includes private keys, public keys, TLS keys, etc.
RUN ${BINS_PATH}/loadgen make-artifacts -c "${CONFIGS_PATH}/loadgen.yaml"

RUN PEER_ORG_PATH="${ARTIFACTS_PATH}/peerOrganizations/peer-org-0" && \
    SERVER_TLS_PATH="${PEER_ORG_PATH}/peers/sidecar-peer-org-0/tls" && \
    CLIENT_TLS_PATH="${PEER_ORG_PATH}/users/client@peer-org-0.com/tls" && \
    ORDERER_TLS_PATH="${ARTIFACTS_PATH}/ordererOrganizations/orderer-org-0/orderers/orderer-0-org-0/tls" && \
    mkdir -p /server-certs /client-certs /orderer-certs && \
    # load the peer's server and client TLS certificates for all services for simplicity.
    ln -s "${SERVER_TLS_PATH}/server.crt" /server-certs/public-key.pem && \
    ln -s "${SERVER_TLS_PATH}/server.key" /server-certs/private-key.pem && \
    ln -s "${SERVER_TLS_PATH}/ca.crt" /server-certs/ca-certificate.pem && \
    ln -s "${CLIENT_TLS_PATH}/client.crt" /client-certs/public-key.pem && \
    ln -s "${CLIENT_TLS_PATH}/client.key" /client-certs/private-key.pem && \
    ln -s "${CLIENT_TLS_PATH}/ca.crt" /client-certs/ca-certificate.pem && \
    # load the orderer's root CA for the loadgen.
    ln -s "${ORDERER_TLS_PATH}/ca.crt" /client-certs/orderer-ca-certificate.pem && \
    # load the orderer's TLS certificates.
    ln -s "${ORDERER_TLS_PATH}/server.crt" /orderer-certs/public-key.pem && \
    ln -s "${ORDERER_TLS_PATH}/server.key" /orderer-certs/private-key.pem && \
    # load the peer's root CA into the orderer so it will be able to verify the peer's creds.
    ln -s "${SERVER_TLS_PATH}/ca.crt" /orderer-certs/ca-certificate.pem

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
