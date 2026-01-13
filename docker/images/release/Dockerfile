#
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#

###########################################
# Stage 1: Production runtime image
###########################################
FROM registry.access.redhat.com/ubi9/ubi-minimal:9.6 AS prod

ARG BIN
ARG ARCHBIN_PATH
ARG TARGETOS
ARG TARGETARCH
ARG PORTS

# Label args
ARG VERSION=1.0
ARG CREATED
ARG REVISION=1.0

# Add non-root user (10001) using BIN argument
RUN /usr/sbin/useradd -u 10001 -r -g root -s /sbin/nologin -c "Hyperledger Fabric-X ${BIN} user" ${BIN} && \
    mkdir -p /home/${BIN} && \
    chown -R 10001:0 /home/${BIN} && \
    chmod 0755 /home/${BIN}

# Copy binaries
COPY ${ARCHBIN_PATH}/${TARGETOS}-${TARGETARCH}/${BIN} /bin/${BIN}

# Create fixed entrypoint since args are not replaced
# within ENTRYPOINT or CMD
RUN ln -s /bin/${BIN} /bin/entrypoint

# Expose ports
EXPOSE ${PORTS}

# OCI metadata labels
LABEL org.opencontainers.image.created="${CREATED}" \
    org.opencontainers.image.description="Hyperledger Fabric-X ${BIN} packaged in a UBI image." \
    org.opencontainers.image.licenses="Apache-2.0" \
    org.opencontainers.image.ref.name="ubi9/ubi-minimal" \
    org.opencontainers.image.revision="${REVISION}" \
    org.opencontainers.image.source="https://github.com/hyperledger/fabric-x-committer" \
    org.opencontainers.image.title="fabric-x-${BIN}" \
    org.opencontainers.image.url="https://github.com/hyperledger/fabric-x-committer" \
    org.opencontainers.image.version="${VERSION}"

# Use non-root user and set workdir using BIN argument
USER 10001
WORKDIR /home/${BIN}

# Default Entrypoint
ENTRYPOINT ["/bin/entrypoint"]
