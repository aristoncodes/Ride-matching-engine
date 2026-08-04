#ifndef COST_MATRIX_H
#define COST_MATRIX_H

#include <vector>
#include "quadtree.h"
#include "assignment.h"
#include "road_graph.h"

// Cost-matrix construction: turn rider/driver GEOMETRY into the MatchEdge list
// the pure solver consumes. This is deliberately the ONLY place that knows
// about coordinates and distances — solveAssignment() sees only edges. That
// boundary is what lets us swap "straight-line distance" for "real road travel
// time" (Week 4's router) later without touching the solver at all.
//
// Riders and drivers are addressed by their INDEX in the input vectors, which
// becomes MatchEdge.rider / MatchEdge.driver. (The Point.id field carries the
// R../D.. label for display and is independent of this index.)
//
// Costs are scaled to integers: euclidean distance * COST_SCALE, rounded. The
// solver needs integer costs (see mcmf.h); COST_SCALE sets how many integer
// units one distance unit is worth — bigger = finer tie-breaking, at the risk
// of overflow on huge grids (long long gives plenty of headroom in practice).
constexpr double COST_SCALE = 1000.0;

// DENSE: every rider connects to every driver. O(N*M) edges. Exact, but the
// edge list itself is O(N*M) to build and feed — fine for small batches,
// the thing the sparse builder exists to avoid at scale.
std::vector<MatchEdge> buildDenseEdges(const std::vector<Point>& riders,
                                       const std::vector<Point>& drivers);

// SPARSE: each rider connects only to its k nearest drivers, found via a
// quadtree over the drivers. O(N*k) edges. A rider whose k-nearest set is
// empty (no drivers at all) simply contributes no edges and ends up unmatched
// — the sparse path's natural rejection mechanism.
//
// gridSize is the side length of the square world the points live in; it sizes
// the quadtree's root boundary. Drivers are indexed by their position in the
// `drivers` vector (that index is stored as the quadtree Point's id).
std::vector<MatchEdge> buildSparseEdges(const std::vector<Point>& riders,
                                        const std::vector<Point>& drivers,
                                        int k, double gridSize);

// ---------------------------------------------------------------------------
// Week 4: the same matrix, priced in real road travel time.
//
// A GeoPoint is a GPS fix, not a grid coordinate: (lat, lon) in degrees.
// Costs come back as SECONDS * COST_SCALE, so the solver is now minimising
// total rider waiting time rather than total map distance. Nothing in
// assignment.cpp or mcmf.cpp changes -- which was the point of keeping
// geometry quarantined in this file.
struct GeoPoint {
    int id;        // display id (R0.., D0..); independent of the vector index
    double lat;
    double lon;
};

// Every rider connected to every driver, priced by driving time.
//
// Costed with ONE BACKWARD Dijkstra PER RIDER, not one A* per pair. A backward
// search from a rider yields the drive time from every node in the city to
// that rider in a single sweep, so an N-rider batch costs N searches instead
// of N*M. At N=M=200 that is 200 searches instead of 40,000.
//
// Direction matters and is easy to get backwards: the driver drives TO the
// rider, so the cost is time(driver -> rider). On a network with one-ways
// those two directions genuinely differ.
//
// A driver who cannot reach a rider at all (wrong side of a severed one-way,
// outside the mapped area) contributes NO EDGE for that pair, which flows
// straight into the existing overflow policy: a rider with no reachable driver
// comes back unmatched (-1) rather than matched to someone who cannot arrive.
std::vector<MatchEdge> buildRoadEdgesDense(const std::vector<GeoPoint>& riders,
                                           const std::vector<GeoPoint>& drivers,
                                           const RoadGraph& graph);

// Road-time costs, but only for each rider's k geometrically nearest drivers.
//
// The candidate SET is still chosen by straight-line distance (the quadtree is
// the only thing fast enough to shortlist at scale), while the PRICE of each
// surviving candidate is real drive time. That split is deliberate: crow-flight
// distance is a poor cost but a decent filter, since the driver who is 400m
// away is rarely beaten by one 5km away however the roads run. Set k with some
// slack -- a k that is too tight lets a bad road layout hide the true best
// driver behind a nearer one.
std::vector<MatchEdge> buildRoadEdgesSparse(const std::vector<GeoPoint>& riders,
                                            const std::vector<GeoPoint>& drivers,
                                            const RoadGraph& graph, int k);

#endif // COST_MATRIX_H
