# The C++ matching engine as a container image.
#
# Multi-stage, because the build needs a full C++ toolchain plus gRPC's
# development headers (~1 GB) and the finished binary needs neither. Shipping
# the builder would mean a compiler in production — a large image, a large
# attack surface, and a slow pull on every deploy.
#
# Image tags are pinned to a digest-stable Debian release rather than :latest,
# so an image built today and one built in six months contain the same
# toolchain. "It works on my machine" is usually "my machine had a different
# base image".

# ---- Stage 1: build ---------------------------------------------------------
FROM debian:bookworm-20250630-slim AS builder

# gRPC and Protobuf come from Debian's archive rather than being built from
# source. Building gRPC takes ~20 minutes; the packaged version is 1.51, which
# is older than the host's 1.83 but implements the same wire protocol — and the
# wire protocol is the actual contract between these processes.
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential=12.9 \
        cmake=3.25.1-1 \
        protobuf-compiler=3.21.12-3+deb12u1 \
        protobuf-compiler-grpc=1.51.1-3+b1 \
        libgrpc++-dev=1.51.1-3+b1 \
        libprotobuf-dev=3.21.12-3+deb12u1 \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# The .proto is copied FIRST and on its own. Docker caches layers by content,
# so as long as the contract has not changed this layer — and the codegen below
# — are reused, and editing a .cpp file does not re-run protoc.
COPY docs/api/matching.proto /src/docs/api/matching.proto

COPY matching_engine/CMakeLists.txt /src/matching_engine/CMakeLists.txt
COPY matching_engine/src /src/matching_engine/src
COPY matching_engine/tests /src/matching_engine/tests
COPY matching_engine/third_party /src/matching_engine/third_party

WORKDIR /src/matching_engine
RUN cmake -B build -S . -DCMAKE_BUILD_TYPE=Release \
    && cmake --build build --target matching_server -j "$(nproc)"

# ---- Stage 2: runtime -------------------------------------------------------
FROM debian:bookworm-20250630-slim AS runtime

# Only the SHARED LIBRARIES the binary actually links against, not the -dev
# packages. This is the difference between a ~1 GB image and a ~150 MB one.
RUN apt-get update && apt-get install -y --no-install-recommends \
        libgrpc++1.51=1.51.1-3+b1 \
        libprotobuf32=3.21.12-3+deb12u1 \
    && rm -rf /var/lib/apt/lists/*

# A non-root user. The engine needs no privileges: it opens one port above 1024,
# reads one file, and writes nothing. Running as root would be a gratuitous
# escalation path out of a container that has nothing to escalate for.
RUN useradd --system --uid 10001 --no-create-home engine

COPY --from=builder /src/matching_engine/build/matching_server /usr/local/bin/matching_server

# A DEFAULT road graph is baked in, and larger ones are mounted over it.
#
# The original version mounted the graph exclusively, which works under Docker
# Compose (there is a host directory to bind) and fails in Kubernetes (there is
# not). A 3.3 MB default on a 179 MB image is negligible and makes the image
# self-contained, so `kubectl apply` works with no volume plumbing at all.
#
# The reasoning behind mounting still holds for anything bigger: a country-sized
# extract is gigabytes and changes on a different schedule from the code, so
# production overrides /data with a PVC or an object-store sidecar. Baked-in is
# the default, not the only option.
RUN mkdir -p /data
COPY matching_engine/data/bengaluru_roads.osm /data/bengaluru_roads.osm
RUN chown -R engine:engine /data

USER engine
EXPOSE 50051

# grpc_health_probe is not installed, so the healthcheck is a plain TCP connect
# via the shell. It proves the listener is up, which is what compose needs for
# `service_healthy` ordering. Deeper readiness — which road graphs are loaded —
# is what the Health RPC is for, and the batcher checks it at startup.
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=5 \
    CMD timeout 2 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/50051" || exit 1

ENTRYPOINT ["/usr/local/bin/matching_server"]
CMD ["--port", "50051"]
