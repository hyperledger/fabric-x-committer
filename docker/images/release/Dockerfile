# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
#
FROM registry.access.redhat.com/ubi9/ubi-micro:9.4 AS prod
ARG BIN
ARG ARCHBIN_PATH
ARG TARGETOS
ARG TARGETARCH
ARG PORTS

# --- Create a non-root user (UBI-micro does not include useradd) ---
USER 10001

# --- Create working directory ---
WORKDIR /app

# --- Copy the architecture-specific binary ---
COPY ${ARCHBIN_PATH}/${TARGETOS}-${TARGETARCH}/${BIN} /bin/${BIN}

# --- Expose ports for Kubernetes/OpenShift ---
EXPOSE ${PORTS}

# --- Container metadata (OCI labels) ---
LABEL name="${BIN}" \
      vendor="IBM" \
      maintainer="ZRL Decentralized Trust Group" \
      description="Production-grade ${BIN} microservice running on UBI-micro" \
      summary="Minimal, secure UBI-micro image for ${BIN}" \
      license="Apache-2.0"

# --- Set runtime entrypoint ---
ENTRYPOINT [/bin/${BIN}]
