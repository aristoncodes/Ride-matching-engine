#ifndef COST_MATRIX_H
#define COST_MATRIX_H

#include <vector>
#include "quadtree.h"
#include "assignment.h"

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

#endif // COST_MATRIX_H
