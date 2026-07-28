#include "quadtree.h"
#include <cmath>
#include <algorithm>
#include <limits>

// 1. Constructor
QuadTree::QuadTree(AABB boundary, int capacity, int depth)
    : boundary(boundary), capacity(capacity < 1 ? 1 : capacity), depth(depth),
      divided(false) {}

// (Destructor is defaulted in the header; unique_ptr children are freed
//  automatically, recursively, and exception-safely.)

// 3. Subdivide: Split the current boundary into 4 equal quadrants
void QuadTree::subdivide() {
    double x = boundary.center_x;
    double y = boundary.center_y;
    double hw = boundary.half_width / 2.0;
    double hh = boundary.half_height / 2.0;

    // Create the 4 child bounding boxes
    AABB nw(x - hw, y + hh, hw, hh); // Northwest
    AABB ne(x + hw, y + hh, hw, hh); // Northeast
    AABB sw(x - hw, y - hh, hw, hh); // Southwest
    AABB se(x + hw, y - hh, hw, hh); // Southeast

    // Instantiate the 4 child QuadTree nodes (one level deeper)
    northwest = std::make_unique<QuadTree>(nw, capacity, depth + 1);
    northeast = std::make_unique<QuadTree>(ne, capacity, depth + 1);
    southwest = std::make_unique<QuadTree>(sw, capacity, depth + 1);
    southeast = std::make_unique<QuadTree>(se, capacity, depth + 1);

    divided = true;
}

// 4. Insert: Add a driver to the tree, subdividing if necessary
bool QuadTree::insert(const Point& p) {
    // If the point is outside this node's boundary, reject it
    if (!boundary.contains(p)) {
        return false;
    }

    // Store here if this leaf has room, OR if we've hit the depth cap (in which
    // case this node becomes a bucket — this is what prevents infinite recursion
    // when more than `capacity` points share (near-)identical coordinates).
    if ((!divided && points.size() < static_cast<size_t>(capacity)) ||
        depth >= MAX_DEPTH) {
        points.push_back(p);
        return true;
    }

    // Node is full. If it hasn't divided yet, do so now.
    if (!divided) {
        subdivide();
    }

    // Try inserting into the children (the quadrants tile the parent exactly,
    // so at least one accepts any point already inside this boundary).
    if (northwest->insert(p)) return true;
    if (northeast->insert(p)) return true;
    if (southwest->insert(p)) return true;
    if (southeast->insert(p)) return true;

    // Unreachable in normal use, but never silently drop a point that was
    // inside our boundary (e.g. a floating-point edge case).
    points.push_back(p);
    return true;
}

// 5. Query: Find all drivers within a specific radius/boundary
void QuadTree::queryRange(const AABB& range, std::vector<Point>& found) const {
    // If the query range doesn't intersect this node's boundary, instantly prune this branch!
    if (!boundary.intersects(range)) {
        return;
    }

    // Check the drivers sitting inside this specific node
    for (const auto& p : points) {
        if (range.contains(p)) {
            found.push_back(p);
        }
    }

    // If this node has children, pass the query down to them
    if (divided) {
        northwest->queryRange(range, found);
        northeast->queryRange(range, found);
        southwest->queryRange(range, found);
        southeast->queryRange(range, found);
    }
}

// 6. Remove: locate the point by id and erase it, then try to shrink the
// tree back down now that it holds one fewer point.
bool QuadTree::remove(const Point& p) {
    if (!boundary.contains(p)) {
        return false;
    }

    // Check points held directly at this node first. insert() only pushes
    // new points into children *after* subdividing — it never redistributes
    // points already sitting in `points` at the moment of subdivision — so a
    // divided (internal) node can still legitimately hold points of its own.
    // Skipping this check (recursing straight into children) would make
    // those points permanently unreachable by remove().
    for (auto it = points.begin(); it != points.end(); ++it) {
        if (it->id == p.id) {
            points.erase(it);
            return true;
        }
    }

    if (!divided) {
        return false;
    }

    bool removed = northwest->remove(p) || northeast->remove(p) ||
                   southwest->remove(p) || southeast->remove(p);
    if (removed) {
        collapseIfPossible();
    }
    return removed;
}

// 7. Collapse: if all 4 children are themselves leaves (not divided) and
// their combined points fit within this node's capacity, pull the points
// back up and delete the children. Only ever collapses one level per call —
// remove() re-checks each ancestor on the way back up the recursion, so a
// chain of empties still fully unwinds after enough removals.
void QuadTree::collapseIfPossible() {
    if (!divided) return;
    if (northwest->divided || northeast->divided ||
        southwest->divided || southeast->divided) {
        return;
    }

    // Include this node's own points (see remove()'s comment: a divided node
    // can still hold points of its own) — both in the capacity check and by
    // appending rather than clearing, so they survive the merge.
    std::size_t childTotal = northwest->points.size() + northeast->points.size() +
                              southwest->points.size() + southeast->points.size();
    if (points.size() + childTotal > static_cast<std::size_t>(capacity)) return;

    for (QuadTree* child : {northwest.get(), northeast.get(), southwest.get(), southeast.get()}) {
        points.insert(points.end(), child->points.begin(), child->points.end());
    }
    northwest.reset();
    northeast.reset();
    southwest.reset();
    southeast.reset();
    divided = false;
}

// 8. Size: total points anywhere in this subtree.
std::size_t QuadTree::size() const {
    std::size_t total = points.size();
    if (divided) {
        total += northwest->size() + northeast->size() +
                 southwest->size() + southeast->size();
    }
    return total;
}

// 9. Clear: reset this node to an empty, undivided leaf.
void QuadTree::clear() {
    points.clear();
    if (divided) {
        northwest.reset();
        northeast.reset();
        southwest.reset();
        southeast.reset();
        divided = false;
    }
}

// 10. Nearest neighbor: queryRange only answers "what's in this box," so a
// true nearest-neighbor search means growing the box until the closest
// candidate found is *provably* closer than anything the box could have
// missed. A point up to sqrt(2)*radius away can sit just outside a box of
// half-extent `radius` (near a corner), so a candidate only counts once its
// true distance is <= radius — otherwise a closer point might still be
// hiding just past the edge, and we double the box and search again.
double QuadTree::leafHalfExtent(double x, double y) const {
    if (!divided) {
        return std::max(boundary.half_width, boundary.half_height);
    }
    if (northwest->boundary.containsXY(x, y)) return northwest->leafHalfExtent(x, y);
    if (northeast->boundary.containsXY(x, y)) return northeast->leafHalfExtent(x, y);
    if (southwest->boundary.containsXY(x, y)) return southwest->leafHalfExtent(x, y);
    if (southeast->boundary.containsXY(x, y)) return southeast->leafHalfExtent(x, y);
    // (x, y) is outside all 4 children (e.g. outside the tree entirely) —
    // fall back to this node's own scale.
    return std::max(boundary.half_width, boundary.half_height);
}

std::optional<Point> QuadTree::nearestNeighbor(double x, double y, int excludeId) const {
    double initialRadius = leafHalfExtent(x, y);
    if (initialRadius <= 0.0) initialRadius = 1.0;
    // Generous upper bound: once the box comfortably covers the whole tree
    // from any interior point, no larger search could find anything new.
    const double maxRadius = (boundary.half_width + boundary.half_height) * 4.0 + 1.0;

    for (double radius = initialRadius; radius <= maxRadius; radius *= 2.0) {
        AABB searchBox(x, y, radius, radius);
        std::vector<Point> candidates;
        queryRange(searchBox, candidates);

        double bestDistSq = std::numeric_limits<double>::max();
        const Point* best = nullptr;
        for (const auto& c : candidates) {
            if (c.id == excludeId) continue;
            double dx = c.x - x;
            double dy = c.y - y;
            double distSq = dx * dx + dy * dy;
            if (distSq < bestDistSq) {
                bestDistSq = distSq;
                best = &c;
            }
        }

        if (best != nullptr && std::sqrt(bestDistSq) <= radius) {
            return *best;
        }
    }
    return std::nullopt;
}

// 11. k nearest neighbors. Same expand-and-retry idea as nearestNeighbor, but
// the stopping condition is stronger: we can only trust that we have the true
// k nearest once we've found at least k candidates AND the k-th nearest is
// within `radius` — otherwise a closer point could still be lurking just
// outside the box, which would displace our current k-th.
std::vector<Point> QuadTree::kNearest(double x, double y, int k) const {
    std::vector<Point> result;
    if (k <= 0) return result;

    double initialRadius = leafHalfExtent(x, y);
    if (initialRadius <= 0.0) initialRadius = 1.0;
    const double maxRadius = (boundary.half_width + boundary.half_height) * 4.0 + 1.0;

    auto distSq = [x, y](const Point& p) {
        double dx = p.x - x, dy = p.y - y;
        return dx * dx + dy * dy;
    };

    for (double radius = initialRadius; ; radius *= 2.0) {
        AABB searchBox(x, y, radius, radius);
        std::vector<Point> candidates;
        queryRange(searchBox, candidates);

        std::sort(candidates.begin(), candidates.end(),
                  [&](const Point& a, const Point& b) { return distSq(a) < distSq(b); });

        bool atMaxRadius = radius >= maxRadius;

        if (static_cast<int>(candidates.size()) >= k) {
            // The k-th nearest so far. If it's within the box, nothing outside
            // could be closer, so these really are the k nearest.
            double kthDist = std::sqrt(distSq(candidates[k - 1]));
            if (kthDist <= radius || atMaxRadius) {
                // Keep the k nearest. (erase, not resize: Point has no default
                // constructor, and resize()'s grow-path would demand one.)
                candidates.erase(candidates.begin() + k, candidates.end());
                return candidates;
            }
        } else if (atMaxRadius) {
            // The whole tree holds fewer than k points; return them all.
            return candidates;
        }
    }
}