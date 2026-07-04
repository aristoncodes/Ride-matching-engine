#ifndef QUADTREE_H
#define QUADTREE_H

#include <vector>
#include <memory>

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

    // Check if a point is inside this specific box
    bool contains(const Point& p) const {
        return (p.x >= center_x - half_width &&
                p.x <= center_x + half_width &&
                p.y >= center_y - half_height &&
                p.y <= center_y + half_height);
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
};

#endif // QUADTREE_H