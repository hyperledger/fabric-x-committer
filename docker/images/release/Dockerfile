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
ARG VERSION=1.0

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
LABEL name="${BIN}" \
    maintainer="IBM Research ZRL Decentralized Trust Group" \
    version="${VERSION}" \
    description="Hyperledger Fabric-X ${BIN} packaged in a UBI image" \
    license="Apache-2.0" \
    vendor="IBM"

# Use non-root user and set workdir using BIN argument
USER 10001
WORKDIR /home/${BIN}

# Default Entrypoint
ENTRYPOINT ["/bin/entrypoint"]
