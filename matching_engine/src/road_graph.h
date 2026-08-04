#ifndef ROAD_GRAPH_H
#define ROAD_GRAPH_H

#include <cstddef>
#include <memory>
#include <span>
#include <unordered_map>
#include <vector>

#include "quadtree.h"

// ============================================================================
// The road network as a directed, weighted graph.
//
// Week 1-3 treated the world as a flat plane and "distance" as a straight line.
// That is a lie a rider can feel: two points 300m apart across a river are a
// 4km drive. From here on, cost is TRAVEL TIME along real roads.
//
// Directed, because one-ways are real. Weighted by SECONDS, not metres,
// because a 2km stretch of motorway is cheaper than 800m of gridlocked
// residential lanes -- and it is time the rider waits, not distance.
// ============================================================================

// Great-circle distance in metres between two lat/lon pairs.
//
// This is also the A* heuristic's backbone: it is the shortest distance
// physically possible between two points on the globe, so no road can ever be
// shorter. That single property is what makes the heuristic admissible.
double haversineMeters(double lat1, double lon1, double lat2, double lon2);

// A junction (or shape point) on the network.
struct GeoNode {
    long long osmId;   // original OpenStreetMap node id, kept for traceability
    double lat;
    double lon;
};

// One directed arc. Length is kept alongside time because the demos want to
// report "5.1 km / 11 min", and because length is what the heuristic bounds.
struct RoadArc {
    int to;
    double travelSeconds;
    double lengthMeters;
};

// Incrementally assembled, then frozen into a RoadGraph. Splitting build from
// use means the finished graph can live in flat CSR arrays (below) instead of
// a vector-of-vectors: one contiguous block, no pointer chasing per neighbour.
// On a graph this size that is the difference between cache hits and misses in
// the innermost loop of every Dijkstra we will ever run.
class RoadGraphBuilder {
public:
    // Returns the dense index for this OSM node, creating it on first sight.
    // Ways share endpoints constantly, so de-duplication here is what actually
    // stitches individual ways into a connected network.
    int addNode(long long osmId, double lat, double lon);

    // Add a single directed arc. A two-way street is simply two calls.
    void addArc(int from, int to, double lengthMeters, double speedMps);

    int numNodes() const { return static_cast<int>(nodes_.size()); }
    std::size_t numArcs() const { return arcs_.size(); }

    friend class RoadGraph;

private:
    struct PendingArc { int from; int to; double travelSeconds; double lengthMeters; };
    std::vector<GeoNode> nodes_;
    std::unordered_map<long long, int> osmToIndex_;
    std::vector<PendingArc> arcs_;
    double maxSpeedMps_ = 0.0;
};

class RoadGraph {
public:
    // Freeze a builder into the immutable CSR form.
    explicit RoadGraph(const RoadGraphBuilder& builder);

    int numNodes() const { return static_cast<int>(nodes_.size()); }
    std::size_t numArcs() const { return arcs_.size(); }
    const GeoNode& node(int v) const { return nodes_[v]; }

    // Outgoing arcs of v, as a contiguous view. This is the hot path of every
    // shortest-path query in the engine.
    std::span<const RoadArc> outgoing(int v) const {
        return {arcs_.data() + outOffsets_[v],
                static_cast<std::size_t>(outOffsets_[v + 1] - outOffsets_[v])};
    }

    // Incoming arcs of v -- the graph with every one-way flipped.
    //
    // Needed because the matching question is "how long until each DRIVER
    // reaches this rider?". Answering that with forward searches costs one
    // Dijkstra per driver; running a single search backwards from the rider
    // answers it for every driver at once. Reversing a directed graph is not
    // optional here -- driving A->B and B->A are genuinely different trips.
    std::span<const RoadArc> incoming(int v) const {
        return {reverseArcs_.data() + inOffsets_[v],
                static_cast<std::size_t>(inOffsets_[v + 1] - inOffsets_[v])};
    }

    // Fastest speed anywhere in the graph, m/s. The A* heuristic divides
    // straight-line distance by THIS (not by the local road's speed): dividing
    // by the global maximum is what keeps the estimate a guaranteed
    // under-estimate, and therefore keeps A* optimal.
    double maxSpeedMps() const { return maxSpeedMps_; }

    // Index of the graph node closest (as the crow flies) to a lat/lon.
    // Riders and drivers report GPS positions that sit in the middle of a
    // building or a road segment, never exactly on a junction, so every query
    // starts by snapping to the network. Backed by the Week 2 quadtree.
    int nearestNode(double lat, double lon) const;

    // Node indices of the largest strongly connected component.
    //
    // "Strongly" matters: with one-ways, u reaching v does not imply v reaches
    // u. Only inside an SCC is every pair mutually reachable. Extracts of a
    // bounded area always contain dead stubs -- roads clipped at the boundary,
    // service driveways -- and a benchmark that keeps drawing unreachable
    // pairs measures failed searches instead of routing.
    const std::vector<int>& largestComponent() const { return largestComponent_; }

    // Bounding box of the network, as (minLat, minLon, maxLat, maxLon).
    void bounds(double& minLat, double& minLon, double& maxLat, double& maxLon) const;

private:
    void buildCsr(const RoadGraphBuilder& builder);
    void computeLargestComponent();

    std::vector<GeoNode> nodes_;
    std::vector<int> outOffsets_;      // size numNodes()+1
    std::vector<RoadArc> arcs_;        // arcs sorted by source node
    std::vector<int> inOffsets_;       // size numNodes()+1
    std::vector<RoadArc> reverseArcs_; // arcs sorted by target node
    double maxSpeedMps_ = 0.0;
    std::vector<int> largestComponent_;
    std::unique_ptr<QuadTree> nodeIndex_;   // spatial index for nearestNode
    double lonScale_ = 1.0;                 // cos(mean latitude), see nearestNode
};

#endif // ROAD_GRAPH_H
