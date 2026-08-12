// The gRPC service layer, tested WITHOUT a socket.
//
// `solveBatchImpl` is the whole request path minus the transport, so these run
// in microseconds and can be exhaustive. What is being pinned here is the
// CONTRACT -- error codes, unmatched reasons, which fields are set under which
// metric -- because those are the things a Go client will depend on and cannot
// see from the .proto alone.

#include "catch.hpp"

#include <string>

#include "graph_registry.h"
#include "matching_service.h"
#include "osm_loader.h"

namespace v1 = matching::v1;

namespace {

// A 12x12 grid, 100 m spacing, as a stand-in road graph. Small, deterministic,
// and every node reachable from every other -- so an UNREACHABLE result in a
// test means the code produced it, not the map.
GraphRegistry gridRegistry(const std::string& id = "test-grid") {
    GraphRegistry registry;
    registry.add(id, std::make_unique<RoadGraph>(buildGridGraph(12, 12, 100.0)));
    return registry;
}

void addRider(v1::MatchBatchRequest& request, const std::string& id, double lat, double lng) {
    v1::Rider* r = request.add_riders();
    r->set_id(id);
    r->mutable_pickup()->set_lat(lat);
    r->mutable_pickup()->set_lng(lng);
}

void addDriver(v1::MatchBatchRequest& request, const std::string& id, double lat, double lng) {
    v1::Driver* d = request.add_candidate_drivers();
    d->set_id(id);
    d->mutable_location()->set_lat(lat);
    d->mutable_location()->set_lng(lng);
}

// Coordinates of a grid cell, for tests that need points ON the graph.
std::pair<double, double> gridPoint(const RoadGraph& graph, int index) {
    const GeoNode& n = graph.node(index);
    return {n.lat, n.lon};
}

v1::UnmatchedReason reasonFor(const v1::MatchBatchResponse& response, const std::string& riderId) {
    for (const v1::UnmatchedRider& u : response.unmatched_riders()) {
        if (u.rider_id() == riderId) return u.reason();
    }
    return v1::UNMATCHED_REASON_UNSPECIFIED;
}

} // namespace

TEST_CASE("an empty batch is success, not an error", "[service][contract]") {
    // Quiet windows are the common case. Making the caller special-case them
    // would push complexity outward for no reason.
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_batch_id("b-empty");
    v1::MatchBatchResponse response;

    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.batch_id() == "b-empty");
    REQUIRE(response.matches_size() == 0);
    REQUIRE(response.unmatched_riders_size() == 0);
}

TEST_CASE("riders with no drivers come back unmatched, not errored", "[service][contract]") {
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    addRider(request, "R-1", 12.97, 77.59);
    addRider(request, "R-2", 12.98, 77.59);
    v1::MatchBatchResponse response;

    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.matches_size() == 0);
    REQUIRE(response.unmatched_riders_size() == 2);
    REQUIRE(reasonFor(response, "R-1") == v1::UNMATCHED_REASON_NO_DRIVER_AVAILABLE);
}

TEST_CASE("euclidean matching pairs riders to their cheapest drivers", "[service]") {
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_EUCLIDEAN);
    addRider(request, "R-north", 12.9800, 77.5900);
    addRider(request, "R-south", 12.9600, 77.5900);
    addDriver(request, "D-south", 12.9601, 77.5900);
    addDriver(request, "D-north", 12.9799, 77.5900);

    v1::MatchBatchResponse response;
    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.matches_size() == 2);
    REQUIRE(response.cost_metric_used() == v1::COST_METRIC_EUCLIDEAN);

    for (const v1::Match& m : response.matches()) {
        if (m.rider_id() == "R-north") REQUIRE(m.driver_id() == "D-north");
        if (m.rider_id() == "R-south") REQUIRE(m.driver_id() == "D-south");

        // straight_line_meters is set under EITHER metric -- it is the field
        // that makes a response auditable.
        REQUIRE(m.straight_line_meters() > 0.0);
        REQUIRE(m.straight_line_meters() < 200.0);

        // No ETA is claimed under euclidean: the engine has no basis for one.
        // A caller must be able to tell "absent" from "zero seconds away".
        REQUIRE_FALSE(m.has_eta_seconds());
    }
    REQUIRE(response.total_cost() > 0.0);
}

TEST_CASE("travel-time matching reports an ETA and echoes the graph used", "[service][road]") {
    const GraphRegistry registry = gridRegistry();
    const RoadGraph& graph = *registry.find("test-grid");
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_TRAVEL_TIME);
    request.set_road_graph_id("test-grid");
    auto [rlat, rlng] = gridPoint(graph, 0);
    auto [dlat, dlng] = gridPoint(graph, 25);
    addRider(request, "R-1", rlat, rlng);
    addDriver(request, "D-1", dlat, dlng);

    v1::MatchBatchResponse response;
    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.matches_size() == 1);

    const v1::Match& m = response.matches(0);
    REQUIRE(m.has_eta_seconds());
    REQUIRE(m.eta_seconds() > 0);
    REQUIRE(m.straight_line_meters() > 0.0);

    // The response states what the engine ACTUALLY did, so a caller tracking an
    // SLO is not just reading its own assumption back.
    REQUIRE(response.cost_metric_used() == v1::COST_METRIC_TRAVEL_TIME);
    REQUIRE(response.road_graph_id() == "test-grid");

    // Road time must be at least the straight-line time: you cannot drive a
    // grid faster than the crow flies across it.
    REQUIRE(static_cast<double>(m.eta_seconds()) >= m.straight_line_meters() / 10.0 - 1.0);
}

TEST_CASE("travel time without a loaded graph fails precondition, never falls back",
          "[service][contract]") {
    // The important half is the SECOND assertion. Silently answering with
    // euclidean would look like success and quietly degrade every ETA the
    // caller reports to a rider -- undetectable from the outside.
    const GraphRegistry registry = gridRegistry();
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_TRAVEL_TIME);
    request.set_road_graph_id("no-such-city");
    addRider(request, "R-1", 12.97, 77.59);
    addDriver(request, "D-1", 12.98, 77.59);

    v1::MatchBatchResponse response;
    const grpc::Status status = service.solveBatchImpl(request, &response);
    REQUIRE(status.error_code() == grpc::StatusCode::FAILED_PRECONDITION);
    REQUIRE(response.matches_size() == 0);
}

TEST_CASE("malformed batches are rejected with INVALID_ARGUMENT", "[service][contract]") {
    const GraphRegistry registry;
    MatchingService service(registry, {});
    v1::MatchBatchResponse response;

    SECTION("empty rider id") {
        v1::MatchBatchRequest request;
        addRider(request, "", 12.97, 77.59);
        addDriver(request, "D-1", 12.98, 77.59);
        REQUIRE(service.solveBatchImpl(request, &response).error_code() ==
                grpc::StatusCode::INVALID_ARGUMENT);
    }
    SECTION("duplicate rider id makes the response ambiguous") {
        v1::MatchBatchRequest request;
        addRider(request, "R-1", 12.97, 77.59);
        addRider(request, "R-1", 12.98, 77.59);
        addDriver(request, "D-1", 12.98, 77.59);
        REQUIRE(service.solveBatchImpl(request, &response).error_code() ==
                grpc::StatusCode::INVALID_ARGUMENT);
    }
    SECTION("duplicate driver id") {
        v1::MatchBatchRequest request;
        addRider(request, "R-1", 12.97, 77.59);
        addDriver(request, "D-1", 12.98, 77.59);
        addDriver(request, "D-1", 12.99, 77.59);
        REQUIRE(service.solveBatchImpl(request, &response).error_code() ==
                grpc::StatusCode::INVALID_ARGUMENT);
    }
    SECTION("latitude out of range") {
        v1::MatchBatchRequest request;
        addRider(request, "R-1", 91.0, 77.59);
        addDriver(request, "D-1", 12.98, 77.59);
        REQUIRE(service.solveBatchImpl(request, &response).error_code() ==
                grpc::StatusCode::INVALID_ARGUMENT);
    }
    SECTION("NaN coordinate would snap to nowhere") {
        v1::MatchBatchRequest request;
        addRider(request, "R-1", std::nan(""), 77.59);
        addDriver(request, "D-1", 12.98, 77.59);
        REQUIRE(service.solveBatchImpl(request, &response).error_code() ==
                grpc::StatusCode::INVALID_ARGUMENT);
    }
    SECTION("negative candidate cap") {
        v1::MatchBatchRequest request;
        request.set_max_candidates_per_rider(-1);
        addRider(request, "R-1", 12.97, 77.59);
        addDriver(request, "D-1", 12.98, 77.59);
        REQUIRE(service.solveBatchImpl(request, &response).error_code() ==
                grpc::StatusCode::INVALID_ARGUMENT);
    }
}

TEST_CASE("an oversized batch is refused rather than attempted", "[service][contract]") {
    // Refusing costs nothing. Attempting commits the memory first and only then
    // discovers it cannot finish in time.
    const GraphRegistry registry;
    MatchingService::Options options;
    options.maxRiders = 3;
    MatchingService service(registry, options);

    v1::MatchBatchRequest request;
    for (int i = 0; i < 4; ++i) addRider(request, "R-" + std::to_string(i), 12.97, 77.59);
    addDriver(request, "D-1", 12.98, 77.59);

    v1::MatchBatchResponse response;
    REQUIRE(service.solveBatchImpl(request, &response).error_code() ==
            grpc::StatusCode::RESOURCE_EXHAUSTED);
}

TEST_CASE("max_pairing_cost keeps a far driver out and says why", "[service][contract]") {
    // The caller's lever for the sum-vs-max problem from Week 4: the solver
    // minimises the total, so without a ceiling it will happily accept one
    // very long wait to shave many short ones.
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_EUCLIDEAN);
    addRider(request, "R-near", 12.9700, 77.5900);
    addRider(request, "R-far", 12.9900, 77.5900);       // ~2.2 km from any driver
    addDriver(request, "D-1", 12.9701, 77.5900);

    request.set_max_pairing_cost(500.0);                // metres, under EUCLIDEAN

    v1::MatchBatchResponse response;
    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.matches_size() == 1);
    REQUIRE(response.matches(0).rider_id() == "R-near");
    REQUIRE(reasonFor(response, "R-far") ==
            v1::UNMATCHED_REASON_ALL_CANDIDATES_ABOVE_CUTOFF);
}

TEST_CASE("a rider no driver can reach is reported UNREACHABLE", "[service][road][contract]") {
    // Two disconnected islands. The distinction matters downstream: the batcher
    // should retry NO_DRIVER_AVAILABLE next window and must NOT retry this,
    // because nothing about the next window will be different.
    GraphRegistry registry;
    {
        RoadGraphBuilder b;
        const int a1 = b.addNode(1, 12.9700, 77.5900);
        const int a2 = b.addNode(2, 12.9710, 77.5900);
        const int b1 = b.addNode(3, 12.9900, 77.6100);
        const int b2 = b.addNode(4, 12.9910, 77.6100);
        b.addArc(a1, a2, 100.0, 10.0);
        b.addArc(a2, a1, 100.0, 10.0);
        b.addArc(b1, b2, 100.0, 10.0);
        b.addArc(b2, b1, 100.0, 10.0);
        registry.add("islands", std::make_unique<RoadGraph>(b));
    }
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_TRAVEL_TIME);
    request.set_road_graph_id("islands");
    addRider(request, "R-islandA", 12.9700, 77.5900);
    addDriver(request, "D-islandB", 12.9900, 77.6100);

    v1::MatchBatchResponse response;
    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.matches_size() == 0);
    REQUIRE(reasonFor(response, "R-islandA") == v1::UNMATCHED_REASON_UNREACHABLE);
}

TEST_CASE("more riders than drivers leaves the surplus unmatched", "[service][contract]") {
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_EUCLIDEAN);
    for (int i = 0; i < 5; ++i) {
        addRider(request, "R-" + std::to_string(i), 12.97 + i * 0.001, 77.59);
    }
    for (int j = 0; j < 2; ++j) {
        addDriver(request, "D-" + std::to_string(j), 12.97 + j * 0.001, 77.5901);
    }

    v1::MatchBatchResponse response;
    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.matches_size() == 2);
    REQUIRE(response.unmatched_riders_size() == 3);
    for (const v1::UnmatchedRider& u : response.unmatched_riders()) {
        REQUIRE(u.reason() == v1::UNMATCHED_REASON_NO_DRIVER_AVAILABLE);
    }

    // Every driver used at most once -- the invariant the flow model exists for,
    // now checked at the API boundary too.
    std::set<std::string> usedDrivers;
    for (const v1::Match& m : response.matches()) {
        REQUIRE(usedDrivers.insert(m.driver_id()).second);
    }
}

TEST_CASE("candidates_per_rider reports what was used, not what was asked",
          "[service][contract]") {
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_EUCLIDEAN);
    for (int i = 0; i < 4; ++i) addRider(request, "R-" + std::to_string(i), 12.97, 77.59 + i * 0.001);
    for (int j = 0; j < 3; ++j) addDriver(request, "D-" + std::to_string(j), 12.971, 77.59 + j * 0.001);

    SECTION("a k at or above the driver count is really dense") {
        request.set_max_candidates_per_rider(50);
        v1::MatchBatchResponse response;
        REQUIRE(service.solveBatchImpl(request, &response).ok());
        REQUIRE(response.candidates_per_rider() == 0);   // 0 == dense
        REQUIRE(response.matches_size() == 3);
    }
    SECTION("a real cap is reported as itself") {
        request.set_max_candidates_per_rider(2);
        v1::MatchBatchResponse response;
        REQUIRE(service.solveBatchImpl(request, &response).ok());
        REQUIRE(response.candidates_per_rider() == 2);
    }
}

TEST_CASE("sparse and dense agree when k covers every driver", "[service]") {
    // A k >= M cannot exclude anything, so the two paths must produce the same
    // assignment. If they diverge, the shortlist is dropping candidates it
    // should have kept.
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest dense;
    dense.set_cost_metric(v1::COST_METRIC_EUCLIDEAN);
    for (int i = 0; i < 6; ++i) addRider(dense, "R-" + std::to_string(i), 12.97 + i * 0.002, 77.59);
    for (int j = 0; j < 6; ++j) addDriver(dense, "D-" + std::to_string(j), 12.97, 77.59 + j * 0.002);

    v1::MatchBatchRequest sparse = dense;
    sparse.set_max_candidates_per_rider(6);

    v1::MatchBatchResponse denseResponse, sparseResponse;
    REQUIRE(service.solveBatchImpl(dense, &denseResponse).ok());
    REQUIRE(service.solveBatchImpl(sparse, &sparseResponse).ok());

    REQUIRE(denseResponse.matches_size() == sparseResponse.matches_size());
    REQUIRE(denseResponse.total_cost() == Approx(sparseResponse.total_cost()));
}

TEST_CASE("compute_micros is populated for SLO tracking", "[service]") {
    const GraphRegistry registry;
    MatchingService service(registry, {});

    v1::MatchBatchRequest request;
    request.set_cost_metric(v1::COST_METRIC_EUCLIDEAN);
    for (int i = 0; i < 40; ++i) addRider(request, "R-" + std::to_string(i), 12.97 + i * 0.001, 77.59);
    for (int j = 0; j < 40; ++j) addDriver(request, "D-" + std::to_string(j), 12.97, 77.59 + j * 0.001);

    v1::MatchBatchResponse response;
    REQUIRE(service.solveBatchImpl(request, &response).ok());
    REQUIRE(response.compute_micros() > 0);
    REQUIRE(response.matches_size() == 40);
}

TEST_CASE("the graph registry reports what is loaded", "[service][health]") {
    // Readiness is not "the process is up". A server whose graph is still
    // parsing must not be sent TRAVEL_TIME traffic, or every call fails.
    const GraphRegistry registry = gridRegistry("blr-central");
    MatchingService service(registry, {});

    v1::HealthRequest request;
    v1::HealthResponse response;
    REQUIRE(service.Health(nullptr, &request, &response).ok());
    REQUIRE(response.healthy());
    REQUIRE(response.loaded_graphs_size() == 1);
    REQUIRE(response.loaded_graphs(0).road_graph_id() == "blr-central");
    REQUIRE(response.loaded_graphs(0).node_count() == 144);
    REQUIRE(response.loaded_graphs(0).arc_count() > 0);
}

TEST_CASE("a duplicate graph id is a startup error", "[service][health]") {
    GraphRegistry registry;
    registry.add("dup", std::make_unique<RoadGraph>(buildGridGraph(3, 3, 100.0)));
    REQUIRE_THROWS_AS(registry.load("dup", "irrelevant.osm"), std::invalid_argument);
}
