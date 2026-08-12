# All three Go services from one multi-stage build.
#
# One Dockerfile rather than three, parameterised by SERVICE, because the build
# steps are identical and three near-copies drift. `docker compose` passes the
# target binary as a build arg.
#
# The runtime stage is `scratch` — literally nothing but the binary. A statically
# linked Go binary needs no libc, no shell, and no package manager, so there is
# nothing in the image for an attacker to use and nothing to patch for CVEs.

# ---- Stage 1: build ---------------------------------------------------------
FROM golang:1.26-bookworm AS builder

# protoc and the Go plugins, so the image builds from the .proto rather than
# from committed generated code. Generated stubs are a build product; a
# checked-in copy is a second source of truth waiting to go stale.
RUN apt-get update && apt-get install -y --no-install-recommends \
        protobuf-compiler=3.21.12-3+deb12u1 \
    && rm -rf /var/lib/apt/lists/*

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2

WORKDIR /src

# Dependencies first and alone. This layer is cached until go.mod/go.sum change,
# so an edit to a .go file does not re-download the module graph.
COPY infrastructure/go.mod infrastructure/go.sum ./infrastructure/
WORKDIR /src/infrastructure
RUN go mod download

WORKDIR /src
COPY docs/api/matching.proto ./docs/api/matching.proto
COPY infrastructure ./infrastructure

RUN mkdir -p infrastructure/gen/matching/v1 \
    && protoc -I docs/api \
        --go_out=infrastructure/gen/matching/v1      --go_opt=paths=source_relative \
        --go-grpc_out=infrastructure/gen/matching/v1 --go-grpc_opt=paths=source_relative \
        docs/api/matching.proto

ARG SERVICE
WORKDIR /src/infrastructure

# CGO_ENABLED=0 is what makes `scratch` possible: without it the binary links
# against the builder's glibc, which does not exist in the runtime image.
# -trimpath strips build paths so the binary is reproducible and leaks no
# filesystem layout. -s -w drop the symbol table and DWARF, ~30% smaller.
RUN test -n "$SERVICE" || (echo "build arg SERVICE is required" && exit 1) \
    && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/service ./cmd/${SERVICE}

# The healthcheck probe, built the same way. See cmd/healthcheck: a scratch
# image has no shell, so `HEALTHCHECK CMD curl ...` cannot run at all.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/healthcheck ./cmd/healthcheck

# ---- Stage 2: runtime -------------------------------------------------------
FROM scratch AS runtime

# CA certificates, copied from the builder. `scratch` has none, and without them
# any future outbound TLS (a webhook, an object store) fails with an opaque
# x509 error. Cheap now, confusing to debug later.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# /etc/passwd with a single non-root entry. `scratch` has no user database, so
# USER 10001 alone would leave the process with a uid the image cannot name —
# which some tooling rejects.
COPY --from=builder /etc/passwd /etc/passwd

COPY --from=builder /out/service /service
COPY --from=builder /out/healthcheck /healthcheck

USER 10001

# Each service overrides the probe target in compose; this default suits the
# two HTTP services. Exec form is mandatory here — the shell form needs /bin/sh,
# which a scratch image does not have.
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=5 \
    CMD ["/healthcheck", "http://127.0.0.1:8080/healthz"]

ENTRYPOINT ["/service"]
