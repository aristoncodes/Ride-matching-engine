#ifndef ASSIGNMENT_H
#define ASSIGNMENT_H

#include <vector>

// One allowed rider->driver pairing and its (integer, pre-scaled) cost.
// Riders and drivers are identified by contiguous indices [0, numRiders) and
// [0, numDrivers) — NOT by their R../D.. string ids. The caller maps back.
//
// Passing edges (rather than a dense matrix) is what lets the SAME solver serve
// both the dense case (every rider-driver pair present) and the sparse case
// (each rider only connected to its k nearest drivers, via the quadtree). The
// solver never needs to know which one it's looking at.
struct MatchEdge {
    int rider;
    int driver;
    long long cost;
};

// Result of an assignment solve.
struct Assignment {
    // riderToDriver[i] = driver index matched to rider i, or -1 if unmatched.
    std::vector<int> riderToDriver;
    long long totalCost;   // sum of matched edge costs (scaled integer units)
    int matchedCount;      // number of riders actually matched
};

// Solve the optimal (minimum total cost) assignment of riders to drivers.
//
// Semantics: MAX matching first, then MIN cost among all max matchings. That
// is, it matches as many riders as the edges allow (at most min(N, M)), and of
// all ways to do that, picks the cheapest. This is min-cost max-flow on:
//
//     source -> each rider      (capacity 1, cost 0)
//     rider  -> driver          (capacity 1, cost = edge cost)   [only given edges]
//     each driver -> sink       (capacity 1, cost 0)
//
// Each driver's single outgoing unit of capacity is what guarantees no driver
// is ever assigned to two riders — the core correctness property.
//
// OVERFLOW POLICY (N > M, or a rider with no candidate edges): such riders come
// back with riderToDriver[i] == -1. This function does NOT decide their fate —
// re-queue-for-next-batch vs. reject-with-signal is a caller/service-layer
// decision (see the Go Match Batcher, Week 12). The solver's contract is only:
// "unmatched riders are reported as -1, never silently dropped or mis-assigned."
Assignment solveAssignment(int numRiders, int numDrivers,
                           const std::vector<MatchEdge>& edges);

#endif // ASSIGNMENT_H
