# Ride-Matching Engine — top-level build entry point.
#
# The one job that genuinely spans both languages is protobuf/gRPC code
# generation: a single .proto produces C++ stubs for the engine and Go stubs
# for the service layer, and the two MUST come from the same file at the same
# revision. Driving that from one place is what stops them drifting.
#
# Generated code is NOT committed (see .gitignore). It is a build product of
# docs/api/matching.proto, and a checked-in copy is just a second source of
# truth waiting to go stale. Building requires protoc + the plugins, which the
# C++ side already needs in order to link against gRPC anyway.

PROTO_DIR   := docs/api
PROTO       := $(PROTO_DIR)/matching.proto

GO_MODULE   := infrastructure
GO_GEN_DIR  := $(GO_MODULE)/gen/matching/v1
CPP_GEN_DIR := matching_engine/gen

GRPC_CPP_PLUGIN := $(shell which grpc_cpp_plugin)
GOBIN           := $(shell go env GOPATH)/bin

.PHONY: all proto proto-go proto-cpp build test test-go test-cpp clean tools check

all: proto build

# ---- Code generation ---------------------------------------------------

proto: proto-go proto-cpp

proto-go: $(PROTO)
	@mkdir -p $(GO_GEN_DIR)
	PATH="$(PATH):$(GOBIN)" protoc -I $(PROTO_DIR) \
		--go_out=$(GO_GEN_DIR)      --go_opt=paths=source_relative \
		--go-grpc_out=$(GO_GEN_DIR) --go-grpc_opt=paths=source_relative \
		$(PROTO)
	@echo "generated Go stubs  -> $(GO_GEN_DIR)"

proto-cpp: $(PROTO)
	@mkdir -p $(CPP_GEN_DIR)
	protoc -I $(PROTO_DIR) \
		--cpp_out=$(CPP_GEN_DIR) \
		--grpc_out=$(CPP_GEN_DIR) \
		--plugin=protoc-gen-grpc=$(GRPC_CPP_PLUGIN) \
		$(PROTO)
	@echo "generated C++ stubs -> $(CPP_GEN_DIR)"

# ---- Build -------------------------------------------------------------

build: proto
	cd matching_engine && cmake -B build -S . > /dev/null && cmake --build build -j
	cd $(GO_MODULE) && go build ./...

# ---- Test --------------------------------------------------------------

test: test-cpp test-go

test-cpp:
	cd matching_engine/build && ctest --output-on-failure

# -race is not optional here: the Go layer is all concurrency, and a data race
# that only shows up under load is exactly what this project must not ship.
test-go:
	cd $(GO_MODULE) && go test -race ./...

# ---- Housekeeping ------------------------------------------------------

# Verifies the toolchain before you waste time on a confusing build error.
check:
	@echo "go              : $$(go version 2>/dev/null || echo MISSING)"
	@echo "protoc          : $$(protoc --version 2>/dev/null || echo MISSING)"
	@echo "grpc_cpp_plugin : $${GRPC_CPP_PLUGIN:-$$(which grpc_cpp_plugin || echo MISSING)}"
	@echo "protoc-gen-go   : $$(ls $(GOBIN)/protoc-gen-go 2>/dev/null || echo MISSING)"
	@echo "grpc++          : $$(pkg-config --modversion grpc++ 2>/dev/null || echo MISSING)"
	@echo "redis-server    : $$(redis-server --version 2>/dev/null || echo MISSING)"

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

clean:
	rm -rf matching_engine/build $(CPP_GEN_DIR) $(GO_MODULE)/gen
