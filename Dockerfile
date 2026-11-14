# syntax=docker/dockerfile:1.29@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183

# ─── builder ────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git make

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/grafana/network-topology-exporter/internal/version.Version=${VERSION} \
      -X github.com/grafana/network-topology-exporter/internal/version.Commit=${COMMIT} \
      -X github.com/grafana/network-topology-exporter/internal/version.BuildDate=${DATE}" \
    -o /out/topology-exporter ./cmd/topology-exporter

# ─── runtime ────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="network-topology-exporter"
LABEL org.opencontainers.image.description="Network topology discovery exporter for Prometheus / Loki / OTLP."
LABEL org.opencontainers.image.licenses="AGPL-3.0-or-later"
LABEL org.opencontainers.image.source="https://github.com/grafana/network-topology-exporter"

USER nonroot:nonroot

COPY --from=builder /out/topology-exporter /usr/local/bin/topology-exporter
COPY config/example.yaml /etc/topology-exporter/config.yaml

EXPOSE 9100

ENTRYPOINT ["/usr/local/bin/topology-exporter"]
CMD ["--config.file=/etc/topology-exporter/config.yaml", "--web.listen-address=:9100"]
