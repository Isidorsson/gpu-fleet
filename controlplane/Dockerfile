# Builds one of the Go binaries: docker build -f controlplane/Dockerfile --build-arg CMD=bmcsim .
# The build context is the repository root, so the attestation reference
# values in deploy/ are reachable. CMD is controlplane, bmcsim or chaos.
FROM golang:1.27.2-bookworm AS builder
ARG CMD
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src/controlplane

# Module download layer: cached until go.mod or go.sum change.
COPY controlplane/go.mod controlplane/go.sum ./
RUN go mod download

COPY controlplane/ ./
# The cache mount shares compiled packages between the three image builds.
RUN --mount=type=cache,target=/root/.cache/go-build \
    : "${CMD:?pass --build-arg CMD=controlplane|bmcsim|chaos}" \
    && test -d "cmd/${CMD}" \
    && go build -trimpath -ldflags="-s -w" -o /out/app "./cmd/${CMD}"

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/app /app
COPY --chmod=a=rX deploy/attestation/reference.json /etc/gpu-fleet/reference.json
USER nonroot:nonroot
ENTRYPOINT ["/app"]
