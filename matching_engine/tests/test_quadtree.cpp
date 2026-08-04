// Quadtree (Week 2) under a real framework.
//
// Spatial structures fail quietly: a wrong nearest neighbour is still a
// plausible-looking point. So almost everything here is checked against a
// linear scan, and the edge cases are the ones that actually broke this code
// during development -- coincident points, boundary points, and remove().

#include "catch.hpp"

#include <random>
#include <set>

#include "oracles.h"
#include "quadtree.h"

namespace {

AABB unitWorld(double size = 100.0) {
    return AABB(size / 2.0, size / 2.0, size / 2.0, size / 2.0);
}

std::vector<Point> randomPoints(int n, double size, unsigned seed) {
    std::mt19937 rng(seed);
    std::uniform_real_distribution<double> dist(0.0, size);
    std::vector<Point> points;
    for (int i = 0; i < n; ++i) points.push_back(Point(i, dist(rng), dist(rng)));
    return points;
}

} // namespace

TEST_CASE("empty tree answers every query without crashing", "[quadtree][edge]") {
    QuadTree tree(unitWorld(), 4);

    REQUIRE(tree.size() == 0);
    REQUIRE_FALSE(tree.nearestNeighbor(50.0, 50.0).has_value());
    REQUIRE(tree.kNearest(50.0, 50.0, 5).empty());

    std::vector<Point> found;
    tree.queryRange(unitWorld(), found);
    REQUIRE(found.empty());

    REQUIRE_FALSE(tree.remove(Point(0, 1.0, 1.0)));
}

TEST_CASE("a single point is found, and only by queries that contain it", "[quadtree][edge]") {
    QuadTree tree(unitWorld(), 4);
    REQUIRE(tree.insert(Point(7, 25.0, 75.0)));
    REQUIRE(tree.size() == 1);

    auto nearest = tree.nearestNeighbor(0.0, 0.0);
    REQUIRE(nearest.has_value());
    REQUIRE(nearest->id == 7);

    // Asking for more than exists returns what exists, not padding.
    REQUIRE(tree.kNearest(0.0, 0.0, 10).size() == 1);

    std::vector<Point> found;
    tree.queryRange(AABB(25.0, 75.0, 1.0, 1.0), found);
    REQUIRE(found.size() == 1);

    found.clear();
    tree.queryRange(AABB(80.0, 10.0, 5.0, 5.0), found);
    REQUIRE(found.empty());

    // Excluding the only point leaves nothing eligible.
    REQUIRE_FALSE(tree.nearestNeighbor(25.0, 75.0, /*excludeId=*/7).has_value());
}

TEST_CASE("points outside the root boundary are rejected", "[quadtree][edge]") {
    QuadTree tree(unitWorld(), 4);
    REQUIRE_FALSE(tree.insert(Point(1, -0.001, 50.0)));
    REQUIRE_FALSE(tree.insert(Point(2, 50.0, 100.001)));
    REQUIRE(tree.size() == 0);
}

TEST_CASE("points exactly on a boundary are stored exactly once", "[quadtree][edge]") {
    QuadTree tree(unitWorld(), 4);

    // The four corners, the centre, and the four edge midpoints. The centre is
    // the interesting one: it lies on the dividing line of all four children,
    // so a containment test with the wrong comparison stores it twice or loses it.
    const std::vector<Point> boundaryPoints = {
        Point(0, 0.0, 0.0),     Point(1, 100.0, 0.0),
        Point(2, 0.0, 100.0),   Point(3, 100.0, 100.0),
        Point(4, 50.0, 50.0),   Point(5, 50.0, 0.0),
        Point(6, 0.0, 50.0),    Point(7, 50.0, 100.0),
        Point(8, 100.0, 50.0),
    };
    for (const Point& p : boundaryPoints) {
        REQUIRE(tree.insert(p));
    }
    REQUIRE(tree.size() == boundaryPoints.size());

    std::vector<Point> found;
    tree.queryRange(unitWorld(), found);
    REQUIRE(found.size() == boundaryPoints.size());

    std::set<int> ids;
    for (const Point& p : found) ids.insert(p.id);
    REQUIRE(ids.size() == boundaryPoints.size());   // no duplicates
}

TEST_CASE("many coincident points do not recurse forever", "[quadtree][edge]") {
    // Every point at the same coordinate can never be separated by subdividing.
    // Without the MAX_DEPTH cap this recurses until the stack dies.
    QuadTree tree(unitWorld(), 4);
    for (int i = 0; i < 500; ++i) {
        REQUIRE(tree.insert(Point(i, 33.0, 66.0)));
    }
    REQUIRE(tree.size() == 500);

    std::vector<Point> found;
    tree.queryRange(AABB(33.0, 66.0, 0.5, 0.5), found);
    REQUIRE(found.size() == 500);

    // All 500 are equidistant; any of them is a correct answer, but there must be one.
    REQUIRE(tree.nearestNeighbor(33.0, 66.0).has_value());
    REQUIRE(tree.kNearest(33.0, 66.0, 10).size() == 10);
}

TEST_CASE("queryRange agrees with a linear scan", "[quadtree]") {
    const std::vector<Point> points = randomPoints(2000, 100.0, 4242);
    QuadTree tree(unitWorld(), 4);
    for (const Point& p : points) REQUIRE(tree.insert(p));
    REQUIRE(tree.size() == points.size());

    std::mt19937 rng(99);
    std::uniform_real_distribution<double> centre(0.0, 100.0);
    std::uniform_real_distribution<double> extent(0.5, 30.0);

    for (int trial = 0; trial < 200; ++trial) {
        const AABB box(centre(rng), centre(rng), extent(rng), extent(rng));

        std::vector<Point> got;
        tree.queryRange(box, got);
        const std::vector<Point> expected = oracle::inRangeByScan(points, box);

        std::set<int> gotIds, expectedIds;
        for (const Point& p : got) gotIds.insert(p.id);
        for (const Point& p : expected) expectedIds.insert(p.id);

        REQUIRE(got.size() == gotIds.size());     // no point reported twice
        REQUIRE(gotIds == expectedIds);
    }
}

TEST_CASE("nearestNeighbor agrees with a linear scan", "[quadtree]") {
    const std::vector<Point> points = randomPoints(2000, 100.0, 777);
    QuadTree tree(unitWorld(), 4);
    for (const Point& p : points) tree.insert(p);

    std::mt19937 rng(31337);
    std::uniform_real_distribution<double> coord(0.0, 100.0);

    for (int trial = 0; trial < 500; ++trial) {
        const double x = coord(rng), y = coord(rng);
        const auto got = tree.nearestNeighbor(x, y);
        const int expectedIndex = oracle::nearestByScan(points, x, y);

        REQUIRE(got.has_value());
        // Compare DISTANCE, not id: ties are legitimately ambiguous.
        REQUIRE(oracle::euclidean(got->x, got->y, x, y) ==
                Approx(oracle::euclidean(points[expectedIndex].x, points[expectedIndex].y, x, y)));
    }
}

TEST_CASE("nearestNeighbor honours excludeId", "[quadtree]") {
    std::vector<Point> points = randomPoints(200, 100.0, 5);
    QuadTree tree(unitWorld(), 4);
    for (const Point& p : points) tree.insert(p);

    // Query from a point that is itself in the tree: without excludeId the
    // answer is the point itself at distance zero, which is useless when
    // re-matching a driver against everyone but themselves.
    const Point& self = points[42];
    const auto withSelf = tree.nearestNeighbor(self.x, self.y);
    REQUIRE(withSelf.has_value());
    REQUIRE(withSelf->id == self.id);

    const auto without = tree.nearestNeighbor(self.x, self.y, self.id);
    REQUIRE(without.has_value());
    REQUIRE(without->id != self.id);

    const int expectedIndex = oracle::nearestByScan(points, self.x, self.y, self.id);
    REQUIRE(oracle::euclidean(without->x, without->y, self.x, self.y) ==
            Approx(oracle::euclidean(points[expectedIndex].x, points[expectedIndex].y,
                                     self.x, self.y)));
}

TEST_CASE("kNearest agrees with a full sort", "[quadtree]") {
    const std::vector<Point> points = randomPoints(1500, 100.0, 2024);
    QuadTree tree(unitWorld(), 4);
    for (const Point& p : points) tree.insert(p);

    std::mt19937 rng(8);
    std::uniform_real_distribution<double> coord(0.0, 100.0);
    std::uniform_int_distribution<int> kDist(1, 20);

    for (int trial = 0; trial < 200; ++trial) {
        const double x = coord(rng), y = coord(rng);
        const int k = kDist(rng);

        const std::vector<Point> got = tree.kNearest(x, y, k);
        const std::vector<Point> expected = oracle::kNearestByScan(points, x, y, k);

        REQUIRE(got.size() == expected.size());
        for (std::size_t i = 0; i < got.size(); ++i) {
            REQUIRE(oracle::euclidean(got[i].x, got[i].y, x, y) ==
                    Approx(oracle::euclidean(expected[i].x, expected[i].y, x, y)));
        }
        // Sorted nearest-first is part of the contract -- the sparse cost
        // matrix relies on it when trimming candidates.
        for (std::size_t i = 1; i < got.size(); ++i) {
            REQUIRE(oracle::euclidean(got[i - 1].x, got[i - 1].y, x, y) <=
                    Approx(oracle::euclidean(got[i].x, got[i].y, x, y)));
        }
    }
}

TEST_CASE("kNearest with a non-positive k returns nothing", "[quadtree][edge]") {
    QuadTree tree(unitWorld(), 4);
    for (const Point& p : randomPoints(50, 100.0, 1)) tree.insert(p);
    REQUIRE(tree.kNearest(10.0, 10.0, 0).empty());
    REQUIRE(tree.kNearest(10.0, 10.0, -3).empty());
}

TEST_CASE("remove takes points out and leaves the rest findable", "[quadtree]") {
    std::vector<Point> points = randomPoints(600, 100.0, 606);
    QuadTree tree(unitWorld(), 4);
    for (const Point& p : points) tree.insert(p);

    SECTION("removing a point that was never inserted fails") {
        REQUIRE_FALSE(tree.remove(Point(99999, 10.0, 10.0)));
        REQUIRE(tree.size() == points.size());
    }

    SECTION("removing every other point keeps the survivors intact") {
        std::vector<Point> survivors;
        for (std::size_t i = 0; i < points.size(); ++i) {
            if (i % 2 == 0) {
                REQUIRE(tree.remove(points[i]));
            } else {
                survivors.push_back(points[i]);
            }
        }
        REQUIRE(tree.size() == survivors.size());

        // Every survivor must still be reachable -- the bug this catches is a
        // collapse that merges children while dropping the parent's own points.
        std::vector<Point> found;
        tree.queryRange(unitWorld(), found);
        REQUIRE(found.size() == survivors.size());

        std::set<int> foundIds, survivorIds;
        for (const Point& p : found) foundIds.insert(p.id);
        for (const Point& p : survivors) survivorIds.insert(p.id);
        REQUIRE(foundIds == survivorIds);

        // And nearest-neighbour must never return something removed.
        for (int trial = 0; trial < 100; ++trial) {
            const auto got = tree.nearestNeighbor(survivors[trial].x, survivors[trial].y);
            REQUIRE(got.has_value());
            REQUIRE(survivorIds.count(got->id) == 1);
        }
    }

    SECTION("removing everything empties the tree") {
        for (const Point& p : points) REQUIRE(tree.remove(p));
        REQUIRE(tree.size() == 0);
        REQUIRE_FALSE(tree.nearestNeighbor(50.0, 50.0).has_value());

        // ...and it is still usable afterwards, which is what a fleet going
        // off-shift and back on actually does to this structure.
        REQUIRE(tree.insert(Point(1234, 50.0, 50.0)));
        REQUIRE(tree.size() == 1);
    }
}

TEST_CASE("insert/remove churn keeps the tree consistent", "[quadtree]") {
    // Drivers going on and off shift, thousands of times. The failure mode is
    // slow corruption, not an immediate crash, so it needs sustained churn.
    QuadTree tree(unitWorld(), 4);
    std::mt19937 rng(4321);
    std::uniform_real_distribution<double> coord(0.0, 100.0);
    std::uniform_int_distribution<int> action(0, 1);

    std::vector<Point> live;
    for (int step = 0; step < 5000; ++step) {
        if (live.empty() || action(rng) == 0) {
            const Point p(step, coord(rng), coord(rng));
            REQUIRE(tree.insert(p));
            live.push_back(p);
        } else {
            std::uniform_int_distribution<std::size_t> pick(0, live.size() - 1);
            const std::size_t idx = pick(rng);
            REQUIRE(tree.remove(live[idx]));
            live.erase(live.begin() + static_cast<long>(idx));
        }
        REQUIRE(tree.size() == live.size());
    }

    std::vector<Point> found;
    tree.queryRange(unitWorld(), found);
    REQUIRE(found.size() == live.size());
}

TEST_CASE("clear resets the tree", "[quadtree][edge]") {
    QuadTree tree(unitWorld(), 4);
    for (const Point& p : randomPoints(300, 100.0, 11)) tree.insert(p);
    REQUIRE(tree.size() == 300);

    tree.clear();
    REQUIRE(tree.size() == 0);
    REQUIRE_FALSE(tree.nearestNeighbor(50.0, 50.0).has_value());

    std::vector<Point> found;
    tree.queryRange(unitWorld(), found);
    REQUIRE(found.empty());
}
