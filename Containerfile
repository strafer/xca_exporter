# 2026.10.24
FROM gcr.io/distroless/static-debian13:nonroot-682887a32ccb0740a70c645019d2582837900dcb@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

ARG BINARY
ARG SOURCE
ARG REVISION

LABEL org.opencontainers.image.title="xca_exporter" \
      org.opencontainers.image.description="Prometheus exporter for XCA" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.revision="${REVISION}"

COPY dist/${BINARY} /xca_exporter

ENTRYPOINT ["/xca_exporter"]
