#ifndef QUADTREE_H
#define QUADTREE_H

#include <vector>
#include <memory>
#include <optional>

// 1. Shared Point Structure
struct Point {
    int id;
    double x;
    double y;

    Point(int _id, double _x, double _y) : id(_id), x(_x), y(_y) {}
};

// 2. Axis-Aligned Bounding Box (AABB)
struct AABB {
    double center_x;
    double center_y;
    double half_width;
    double half_height;

    AABB(double cx, double cy, double hw, double hh)
        : center_x(cx), center_y(cy), half_width(hw), half_height(hh) {}

    // Check if a raw (x, y) is inside this specific box.
    bool containsXY(double x, double y) const {
        return (x >= center_x - half_width &&
                x <= center_x + half_width &&
                y >= center_y - half_height &&
                y <= center_y + half_height);
    }

    // Check if a point is inside this specific box
    bool contains(const Point& p) const {
        return containsXY(p.x, p.y);
    }

    // Check if this box overlaps with another box (used for searching later)
    bool intersects(const AABB& other) const {
        return !(other.center_x - other.half_width > center_x + half_width ||
                 other.center_x + other.half_width < center_x - half_width ||
                 other.center_y - other.half_height > center_y + half_height ||
                 other.center_y + other.half_height < center_y - half_height);
    }
};

// 3. The QuadTree Node Class
class QuadTree {
private:
    AABB boundary;
    int capacity;
    int depth;
    std::vector<Point> points;
    bool divided;

    // Owning pointers to the 4 child regions. unique_ptr gives automatic
    // cleanup (no manual destructor) and makes accidental copies a compile error.
    std::unique_ptr<QuadTree> northwest;
    std::unique_ptr<QuadTree> northeast;
    std::unique_ptr<QuadTree> southwest;
    std::unique_ptr<QuadTree> southeast;

    // Hard cap on subdivision to prevent unbounded recursion when more than
    // `capacity` points share (near-)identical coordinates. A node at this
    // depth becomes a bucket that accepts points without subdividing further.
    static constexpr int MAX_DEPTH = 16;

public:
    // Constructor (destructor is defaulted: unique_ptr cleans up children)
    QuadTree(AABB boundary, int capacity, int depth = 0);
    ~QuadTree() = default;

    // Owning raw resources by value semantics would double-free; forbid copies.
    QuadTree(const QuadTree&) = delete;
    QuadTree& operator=(const QuadTree&) = delete;

    // Core behaviors
    bool insert(const Point& p);
    void subdivide();
    void queryRange(const AABB& range, std::vector<Point>& found) const;

    // Remove the point matching p.id. Returns false if no such point is in
    // the tree. After a removal, collapses any subtree of 4 undivided
    // children back into their parent once it can hold all their points —
    // otherwise long-running insert/remove churn (drivers going on/off
    // shift) would leave the tree permanently bloated with near-empty nodes.
    bool remove(const Point& p);

    // Total points currently stored anywhere in this subtree.
    std::size_t size() const;

    // Drop every point and child, resetting this node to a fresh leaf.
    void clear();

    // Find the point nearest to (x, y). Hides the expand-and-retry box
    // search behind one call: queryRange only answers "what's in this box",
    // so finding a true nearest neighbor means searching a growing box until
    // the closest candidate found is provably closer than the box's edge.
    // Pass excludeId to skip a point already known to be at (x, y) itself
    // (e.g. when re-matching a point that is itself stored in the tree).
    // Returns std::nullopt if the tree contains no eligible point.
    std::optional<Point> nearestNeighbor(double x, double y, int excludeId = -1) const;

    // Return the k points nearest to (x, y), sorted nearest-first. Fewer than
    // k are returned only if the tree holds fewer than k points. This is the
    // engine behind the *sparse* cost matrix: instead of connecting every
    // rider to every driver (O(N*M) edges), each rider gets edges to just its
    // k nearest drivers (O(N*k)) — the difference between a matrix that's
    // expensive to even build and one that holds sub-millisecond at scale.
    std::vector<Point> kNearest(double x, double y, int k) const;

private:
    // Merge 4 undivided children back into this node if their combined
    // points now fit within `capacity`. No-op if any child is itself
    // divided (we only ever collapse one level at a time).
    void collapseIfPossible();

    // Descend to the leaf that would contain (x, y) and return its
    // half-extent. Used to seed nearestNeighbor's starting search radius at
    // local scale (small in dense regions, large in sparse ones) instead of
    // a grid-wide constant that degrades to an O(N) scan under dense data.
    double leafHalfExtent(double x, double y) const;
};

#endif // QUADTREE_H