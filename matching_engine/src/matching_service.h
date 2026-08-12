#ifndef MATCHING_SERVICE_H
#define MATCHING_SERVICE_H

#include <grpcpp/grpcpp.h>

#include <string>

#include "graph_registry.h"
#include "matching.grpc.pb.h"

// The gRPC service: docs/api/matching.proto, implemented on top of the Week 1-5
// engine.
//
// Deliberately thin. Everything it does falls into four steps -- validate,
// build a cost matrix, solve, translate back -- and NONE of the algorithmic
// work lives here. `solveAssignment` and the routers are untouched by the fact
// that they are now behind a network. That is the same boundary discipline
// that let Week 4 swap the entire cost model without touching the solver.
//
// Concurrency: gRPC serves requests on a thread pool, so `SolveBatch` runs
// concurrently on many threads. This class holds NO mutable state -- the graph
// registry is read-only after startup, and every per-request value is a local.
// That is why there is no mutex anywhere in it: statelessness is what makes it
// safe, and it is also what lets the C++ tier scale as an anonymous worker pool.
class MatchingService final : public matching::v1::MatchingEngine::Service {
public:
    struct Options {
        // Reject batches above this size with RESOURCE_EXHAUSTED rather than
        // attempting them. An unbounded batch is an unbounded solve time and
        // an unbounded allocation -- a deadline would eventually fire, but only
        // after the memory was already committed.
        int maxRiders = 5000;
        int maxDrivers = 5000;
        std::string version = "dev";
    };

    MatchingService(const GraphRegistry& graphs, Options options);

    grpc::Status SolveBatch(grpc::ServerContext* context,
                            const matching::v1::MatchBatchRequest* request,
                            matching::v1::MatchBatchResponse* response) override;

    grpc::Status Health(grpc::ServerContext* context,
                        const matching::v1::HealthRequest* request,
                        matching::v1::HealthResponse* response) override;

    // The same logic without a ServerContext, so tests can drive it directly
    // instead of standing up a socket. Passing a null context is exactly what
    // SolveBatch does when there is nothing to cancel against.
    grpc::Status solveBatchImpl(const matching::v1::MatchBatchRequest& request,
                                matching::v1::MatchBatchResponse* response,
                                grpc::ServerContext* context = nullptr);

private:
    const GraphRegistry& graphs_;
    Options options_;
};

#endif // MATCHING_SERVICE_H
