#include "matching_service.h"

#include <chrono>
#include <cmath>
#include <unordered_set>
#include <vector>

#include "assignment.h"
#include "cost_matrix.h"
#include "road_graph.h"
#include "router.h"

namespace v1 = matching::v1;

namespace {

using Clock = std::chrono::steady_clock;

constexpr double kMetersPerDegreeLat = 111320.0;
constexpr double kPi = 3.14159265358979323846;

// Reject a batch we cannot interpret, rather than guessing. Every check here
// is something that would otherwise produce a confidently wrong answer:
// a NaN coordinate silently becomes the nearest node to nowhere, and a
// duplicate rider id makes the response ambiguous to the caller.
grpc::Status validate(const v1::MatchBatchRequest& request,
                      const MatchingService::Options& options) {
    if (static_cast<int>(request.riders().size()) > options.maxRiders ||
        static_cast<int>(request.candidate_drivers().size()) > options.maxDrivers) {
        return {grpc::StatusCode::RESOURCE_EXHAUSTED,
                "batch exceeds server limits (max " + std::to_string(options.maxRiders) +
                    " riders, " + std::to_string(options.maxDrivers) + " drivers)"};
    }

    auto checkCoord = [](const v1::LatLng& p, const std::string& who) -> grpc::Status {
        if (!std::isfinite(p.lat()) || !std::isfinite(p.lng())) {
            return {grpc::StatusCode::INVALID_ARGUMENT, who + " has a non-finite coordinate"};
        }
        if (p.lat() < -90.0 || p.lat() > 90.0 || p.lng() < -180.0 || p.lng() > 180.0) {
            return {grpc::StatusCode::INVALID_ARGUMENT, who + " coordinate out of range"};
        }
        return grpc::Status::OK;
    };

    std::unordered_set<std::string> riderIds;
    for (const v1::Rider& r : request.riders()) {
        if (r.id().empty()) {
            return {grpc::StatusCode::INVALID_ARGUMENT, "rider with empty id"};
        }
        if (!riderIds.insert(r.id()).second) {
            return {grpc::StatusCode::INVALID_ARGUMENT, "duplicate rider id: " + r.id()};
        }
        if (grpc::Status s = checkCoord(r.pickup(), "rider " + r.id()); !s.ok()) return s;
    }

    std::unordered_set<std::string> driverIds;
    for (const v1::Driver& d : request.candidate_drivers()) {
        if (d.id().empty()) {
            return {grpc::StatusCode::INVALID_ARGUMENT, "driver with empty id"};
        }
        if (!driverIds.insert(d.id()).second) {
            return {grpc::StatusCode::INVALID_ARGUMENT, "duplicate driver id: " + d.id()};
        }
        if (grpc::Status s = checkCoord(d.location(), "driver " + d.id()); !s.ok()) return s;
    }

    if (request.max_candidates_per_rider() < 0) {
        return {grpc::StatusCode::INVALID_ARGUMENT, "max_candidates_per_rider must be >= 0"};
    }
    if (request.max_pairing_cost() < 0.0 || !std::isfinite(request.max_pairing_cost())) {
        return {grpc::StatusCode::INVALID_ARGUMENT, "max_pairing_cost must be finite and >= 0"};
    }
    return grpc::Status::OK;
}

// Project lat/lng to a local metric plane so the euclidean path's costs come
// out in METRES rather than degrees. Equirectangular around the batch's mean
// latitude: accurate to well under a percent over a city, and it keeps the
// quadtree's plain euclidean metric meaningful.
struct Projection {
    double meanLat = 0.0;
    double lonScale = 1.0;

    Point project(int id, double lat, double lng) const {
        return Point(id, lng * lonScale * kMetersPerDegreeLat, lat * kMetersPerDegreeLat);
    }
};

Projection makeProjection(const v1::MatchBatchRequest& request) {
    double sum = 0.0;
    int n = 0;
    for (const v1::Rider& r : request.riders())            { sum += r.pickup().lat(); ++n; }
    for (const v1::Driver& d : request.candidate_drivers()) { sum += d.location().lat(); ++n; }
    Projection p;
    p.meanLat = n ? sum / n : 0.0;
    p.lonScale = std::cos(p.meanLat * kPi / 180.0);
    return p;
}

} // namespace

MatchingService::MatchingService(const GraphRegistry& graphs, Options options)
    : graphs_(graphs), options_(std::move(options)) {}

grpc::Status MatchingService::SolveBatch(grpc::ServerContext* context,
                                         const v1::MatchBatchRequest* request,
                                         v1::MatchBatchResponse* response) {
    return solveBatchImpl(*request, response, context);
}

grpc::Status MatchingService::solveBatchImpl(const v1::MatchBatchRequest& request,
                                             v1::MatchBatchResponse* response,
                                             grpc::ServerContext* context) {
    const auto started = Clock::now();
    response->set_batch_id(request.batch_id());

    if (grpc::Status status = validate(request, options_); !status.ok()) return status;

    const int numRiders = request.riders().size();
    const int numDrivers = request.candidate_drivers().size();

    const bool useTravelTime = request.cost_metric() == v1::COST_METRIC_TRAVEL_TIME;
    const RoadGraph* graph = nullptr;
    if (useTravelTime) {
        graph = graphs_.find(request.road_graph_id());
        if (graph == nullptr) {
            // Explicitly NOT falling back to euclidean. A silent downgrade of
            // the cost model is invisible to the caller and quietly degrades
            // every ETA it reports to a rider.
            return {grpc::StatusCode::FAILED_PRECONDITION,
                    "road graph '" + request.road_graph_id() +
                        "' is not loaded; cannot serve COST_METRIC_TRAVEL_TIME"};
        }
    }

    response->set_cost_metric_used(useTravelTime ? v1::COST_METRIC_TRAVEL_TIME
                                                 : v1::COST_METRIC_EUCLIDEAN);
    response->set_road_graph_id(useTravelTime ? request.road_graph_id() : "");

    // An empty batch is a normal quiet window, not an error. Answering it with
    // an empty assignment means the caller needs no special case for the most
    // common thing that can happen.
    if (numRiders == 0 || numDrivers == 0) {
        for (const v1::Rider& r : request.riders()) {
            v1::UnmatchedRider* u = response->add_unmatched_riders();
            u->set_rider_id(r.id());
            u->set_reason(v1::UNMATCHED_REASON_NO_DRIVER_AVAILABLE);
        }
        response->set_compute_micros(
            std::chrono::duration_cast<std::chrono::microseconds>(Clock::now() - started).count());
        return grpc::Status::OK;
    }

    // k = 0 means "no cap", i.e. a dense matrix. A k at or above the driver
    // count is also effectively dense, and reporting it as such is what makes
    // candidates_per_rider in the response the truth rather than an echo.
    int k = request.max_candidates_per_rider();
    if (k >= numDrivers) k = 0;
    const int shortlistSize = (k == 0) ? numDrivers : k;
    response->set_candidates_per_rider(k);

    // ---- Build the cost matrix -----------------------------------------
    std::vector<MatchEdge> edges;
    const Projection projection = makeProjection(request);
    std::vector<Point> flatRiders, flatDrivers;   // euclidean path
    std::vector<GeoPoint> geoRiders, geoDrivers;  // road path

    if (useTravelTime) {
        geoRiders.reserve(numRiders);
        geoDrivers.reserve(numDrivers);
        for (int i = 0; i < numRiders; ++i) {
            const v1::LatLng& p = request.riders(i).pickup();
            geoRiders.push_back(GeoPoint{i, p.lat(), p.lng()});
        }
        for (int j = 0; j < numDrivers; ++j) {
            const v1::LatLng& p = request.candidate_drivers(j).location();
            geoDrivers.push_back(GeoPoint{j, p.lat(), p.lng()});
        }
        edges = (k == 0) ? buildRoadEdgesDense(geoRiders, geoDrivers, *graph)
                         : buildRoadEdgesSparse(geoRiders, geoDrivers, *graph, k);
    } else {
        flatRiders.reserve(numRiders);
        flatDrivers.reserve(numDrivers);
        for (int i = 0; i < numRiders; ++i) {
            const v1::LatLng& p = request.riders(i).pickup();
            flatRiders.push_back(projection.project(i, p.lat(), p.lng()));
        }
        for (int j = 0; j < numDrivers; ++j) {
            const v1::LatLng& p = request.candidate_drivers(j).location();
            flatDrivers.push_back(projection.project(j, p.lat(), p.lng()));
        }
        if (k == 0) {
            edges = buildDenseEdges(flatRiders, flatDrivers);
        } else {
            // buildSparseEdges sizes its quadtree from a square world; give it
            // one that certainly encloses every projected point.
            double maxCoord = 0.0;
            for (const Point& p : flatRiders)  maxCoord = std::max({maxCoord, p.x, p.y});
            for (const Point& p : flatDrivers) maxCoord = std::max({maxCoord, p.x, p.y});
            edges = buildSparseEdges(flatRiders, flatDrivers, k, maxCoord * 2.0 + 1.0);
        }
    }

    // Cancellation check between the two expensive phases. Building the matrix
    // is the slow half under TRAVEL_TIME; if the caller has already given up
    // there is no point paying for the solve as well.
    if (context != nullptr && context->IsCancelled()) {
        return {grpc::StatusCode::CANCELLED, "cancelled by client before solve"};
    }

    // ---- Apply the per-rider cost ceiling ------------------------------
    // Counted per rider before and after, because the DIFFERENCE is what tells
    // the caller why a rider went unmatched -- and that is the one thing the
    // service layer needs in order to choose re-queue vs reject.
    std::vector<int> consideredPerRider(numRiders, 0);
    std::vector<int> keptPerRider(numRiders, 0);
    for (const MatchEdge& e : edges) ++consideredPerRider[e.rider];

    const double cutoff = request.max_pairing_cost();
    if (cutoff > 0.0) {
        const long long scaledCutoff = static_cast<long long>(std::llround(cutoff * COST_SCALE));
        std::vector<MatchEdge> kept;
        kept.reserve(edges.size());
        for (const MatchEdge& e : edges) {
            if (e.cost <= scaledCutoff) kept.push_back(e);
        }
        edges = std::move(kept);
    }
    for (const MatchEdge& e : edges) ++keptPerRider[e.rider];

    // ---- Solve ----------------------------------------------------------
    const Assignment assignment = solveAssignment(numRiders, numDrivers, edges);

    // Cost of each rider's WINNING edge, recovered in a single pass.
    //
    // The obvious version -- scan `edges` for the matched pair inside the
    // per-rider loop below -- is O(riders x edges). Harmless at N=100, and
    // 125 billion comparisons on a dense 5000-rider batch, which the server
    // explicitly allows. One pass here instead.
    std::vector<long long> matchedCost(numRiders, 0);
    for (const MatchEdge& e : edges) {
        if (assignment.riderToDriver[e.rider] == e.driver) {
            matchedCost[e.rider] = e.cost;
        }
    }

    // ---- Translate back --------------------------------------------------
    for (int i = 0; i < numRiders; ++i) {
        const v1::Rider& rider = request.riders(i);
        const int j = assignment.riderToDriver[i];

        if (j < 0) {
            v1::UnmatchedRider* u = response->add_unmatched_riders();
            u->set_rider_id(rider.id());
            // Ordered most-specific first: each branch rules out the ones below.
            if (shortlistSize == 0) {
                u->set_reason(v1::UNMATCHED_REASON_NO_CANDIDATES_IN_SHORTLIST);
            } else if (consideredPerRider[i] == 0) {
                // Candidates existed geometrically but produced no edge at all,
                // which only happens when none of them can reach the rider.
                u->set_reason(useTravelTime ? v1::UNMATCHED_REASON_UNREACHABLE
                                            : v1::UNMATCHED_REASON_NO_CANDIDATES_IN_SHORTLIST);
            } else if (keptPerRider[i] == 0) {
                u->set_reason(v1::UNMATCHED_REASON_ALL_CANDIDATES_ABOVE_CUTOFF);
            } else {
                // This rider had usable options; the batch simply ran out of
                // drivers, or giving it one would have cost the batch more.
                u->set_reason(v1::UNMATCHED_REASON_NO_DRIVER_AVAILABLE);
            }
            continue;
        }

        const v1::Driver& driver = request.candidate_drivers(j);
        v1::Match* match = response->add_matches();
        match->set_rider_id(rider.id());
        match->set_driver_id(driver.id());

        // Always set: computable under either metric, and it is what makes a
        // response auditable (a 40 s ETA across a 6 km gap is visibly wrong).
        match->set_straight_line_meters(haversineMeters(
            rider.pickup().lat(), rider.pickup().lng(),
            driver.location().lat(), driver.location().lng()));

        if (useTravelTime) {
            // The solved edge cost IS the driving time, so the ETA is free.
            // road_distance_meters is deliberately left unset: recovering it
            // means routing each matched pair a second time (~0.6 ms each,
            // doubling batch cost) for a number the caller does not need in
            // order to dispatch. The field stays optional precisely so absent
            // is a legal, readable answer.
            match->set_eta_seconds(
                static_cast<int>(std::llround(matchedCost[i] / COST_SCALE)));
        }
    }

    response->set_total_cost(static_cast<double>(assignment.totalCost) / COST_SCALE);
    response->set_compute_micros(
        std::chrono::duration_cast<std::chrono::microseconds>(Clock::now() - started).count());
    return grpc::Status::OK;
}

grpc::Status MatchingService::Health(grpc::ServerContext*, const v1::HealthRequest*,
                                     v1::HealthResponse* response) {
    response->set_healthy(true);
    response->set_version(options_.version);
    for (const std::string& id : graphs_.ids()) {
        const RoadGraph* graph = graphs_.find(id);
        v1::LoadedGraph* loaded = response->add_loaded_graphs();
        loaded->set_road_graph_id(id);
        loaded->set_node_count(graph->numNodes());
        loaded->set_arc_count(static_cast<int>(graph->numArcs()));
    }
    return grpc::Status::OK;
}
