#include "quadtree.h"

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