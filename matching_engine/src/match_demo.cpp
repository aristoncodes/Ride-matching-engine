// Interim matching demo: exercises the QuadTree by matching each rider to its
// nearest driver (greedy nearest-neighbor, box-query + expand-and-retry).
//
// NOTE: this is a placeholder that lives OUTSIDE the Week 1 generator on
// purpose (separation of concerns). The real, globally-optimal matcher is the
// Week 3 deliverable (Hungarian / MCMF); this greedy version is kept only as a
// baseline / test oracle. See docs/adr/0003-optimal-matching-over-greedy.md.

#include <iostream>
#include <vector>
#include <random>
#include <limits>
#include <cmath>
#include "quadtree.h"

int main() {
    const int    N         = 1000;    // riders
    const int    M         = 100;     // drivers
    const double GRID_SIZE = 1000.0;

    std::random_device rd;
    std::mt19937 gen(rd());
    std::uniform_real_distribution<double> dis(0.0, GRID_SIZE);

    std::vector<Point> riders;
    riders.reserve(N);
    for (int i = 0; i < N; ++i) riders.emplace_back(i, dis(gen), dis(gen));

    std::vector<Point> drivers;
    drivers.reserve(M);
    for (int i = 0; i < M; ++i) drivers.emplace_back(i, dis(gen), dis(gen));

    // Index all drivers in a QuadTree covering the whole grid. AABB is
    // (center_x, center_y, half_width, half_height), so the root is centered
    // on the grid with a half-extent of GRID_SIZE/2.
    AABB worldBounds(GRID_SIZE / 2.0, GRID_SIZE / 2.0,
                     GRID_SIZE / 2.0, GRID_SIZE / 2.0);
    QuadTree driverTree(worldBounds, 4);
    for (const auto& d : drivers) driverTree.insert(d);

    // Match each rider to its nearest driver.
    for (const auto& rider : riders) {
        int    bestId     = -1;
        double bestDistSq = std::numeric_limits<double>::max();

        // queryRange returns everything inside a BOX; search a box of
        // half-extent `radius`, filter by true distance, and grow the box if
        // empty (or if the best match is farther than the radius searched).
        for (double radius = GRID_SIZE / 20.0;
             bestId == -1 && radius <= GRID_SIZE * 2.0;
             radius *= 2.0) {

            AABB searchBox(rider.x, rider.y, radius, radius);
            std::vector<Point> candidates;
            driverTree.queryRange(searchBox, candidates);

            for (const auto& d : candidates) {
                double dx = d.x - rider.x;
                double dy = d.y - rider.y;
                double distSq = dx * dx + dy * dy;
                if (distSq < bestDistSq) {
                    bestDistSq = distSq;
                    bestId = d.id;
                }
            }
            // A box corner is ~1.41*radius away, and a closer driver could sit
            // just outside the box, so only trust a match within `radius`.
            if (bestId != -1 && std::sqrt(bestDistSq) > radius) {
                bestId = -1;
                bestDistSq = std::numeric_limits<double>::max();
            }
        }

        if (bestId != -1) {
            std::cout << "Rider R" << rider.id << " -> Driver D" << bestId
                      << " (distance " << std::sqrt(bestDistSq) << ")\n";
        } else {
            std::cout << "Rider R" << rider.id << " -> no driver found\n";
        }
    }

    return 0;
}
