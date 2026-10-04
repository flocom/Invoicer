# syntax=docker/dockerfile:1.7
# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=v0.0.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X github.com/flocom/invoicer/internal/config.Version=${VERSION}" \
      -o /out/invoicer ./cmd/invoicer \
 && mkdir -p /out/data

# ---- runtime: distroless, static, non-root, no shell ----
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/flocom/Invoicer" \
      org.opencontainers.image.description="Self-hosted invoicing: multi-company, Stripe, Resend, recurring invoices" \
      org.opencontainers.image.licenses="NOASSERTION"
COPY --from=build --chown=65532:65532 /out/invoicer /app/invoicer
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV INVOICER_DATA=/data
VOLUME ["/data"]
EXPOSE 8080 8443
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/app/invoicer", "healthcheck"]
ENTRYPOINT ["/app/invoicer"]
