FROM rust:1.93-alpine AS builder
RUN apk add --no-cache musl-dev
WORKDIR /build

# Build dependencies against a stub main first so this layer is cached until
# Cargo.toml or Cargo.lock change.
COPY Cargo.toml Cargo.lock ./
RUN mkdir src \
    && echo 'fn main() {}' > src/main.rs \
    && cargo build --release --locked

# COPY keeps old mtimes, so touch the sources to force cargo to rebuild the
# crate instead of reusing the stub binary.
COPY src ./src
RUN find src -type f -exec touch {} + \
    && cargo build --release --locked

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /build/target/release/gpu-agent /gpu-agent
EXPOSE 9400
USER nonroot:nonroot
ENTRYPOINT ["/gpu-agent"]
