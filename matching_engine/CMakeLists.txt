# 1. Always set the minimum required version first
cmake_minimum_required(VERSION 3.10)

# 2. Then call the project() command
project(RideMatchingEngine)

# 3. Then set your standards
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# Default to an optimised build. Without this CMake picks no build type at all,
# which means -O0 -- and every timing number in the benchmarks becomes a
# measurement of unoptimised code rather than of the algorithm.
if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
    set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

# Week 5 stretch: AddressSanitizer + UndefinedBehaviorSanitizer.
#   cmake -DENABLE_SANITIZERS=ON .. && make && ctest
# ASan catches the memory bugs the compiler allows (use-after-free in the
# quadtree's unique_ptr churn, reads past the end of a CSR span); UBSan catches
# signed overflow and the undefined casts that quietly work until they do not.
# Roughly 2-3x slower, so the timing assertions are skipped under it.
option(ENABLE_SANITIZERS "Build with AddressSanitizer and UndefinedBehaviorSanitizer" OFF)
if(ENABLE_SANITIZERS)
    add_compile_options(-fsanitize=address,undefined -fno-omit-frame-pointer -g)
    add_link_options(-fsanitize=address,undefined)
    message(STATUS "Sanitizers enabled (ASan + UBSan); [perf] assertions will be skipped")
endif()

# 4. Executables
#    generator       — Week 1: standalone random coordinate generator (no deps)
#    match_demo      — interim greedy nearest-neighbor demo exercising the QuadTree
#    benchmark       — Week 2 stretch: quadtree vs brute-force nearest-neighbor timing
#    optimal_match   — Week 3 showcase: greedy vs optimal (dense/sparse), overflow
#    route_demo      — Week 4: Dijkstra vs A* on a real OSM road graph
#    road_match_demo — Week 4: matching on road travel time vs straight-line distance
add_executable(generator src/generator.cpp)
add_executable(match_demo src/match_demo.cpp src/quadtree.cpp)
add_executable(benchmark src/benchmark.cpp src/quadtree.cpp)
add_executable(optimal_match src/optimal_match.cpp src/assignment.cpp src/mcmf.cpp
               src/cost_matrix.cpp src/quadtree.cpp src/road_graph.cpp src/router.cpp)
add_executable(route_demo src/route_demo.cpp src/router.cpp src/road_graph.cpp
               src/osm_loader.cpp src/quadtree.cpp)
add_executable(road_match_demo src/road_match_demo.cpp src/assignment.cpp src/mcmf.cpp
               src/cost_matrix.cpp src/quadtree.cpp src/road_graph.cpp src/router.cpp
               src/osm_loader.cpp)

# 5. Week 5: the test suite.
#
# Catch2 v2's single header is vendored in third_party/ rather than fetched at
# configure time, so a clean checkout builds and tests offline -- including in
# a CI container with no network egress. v2 (not v3) precisely because it is
# one header with no build system of its own.
add_executable(unit_tests
    tests/test_main.cpp
    tests/test_quadtree.cpp
    tests/test_assignment.cpp
    tests/test_router.cpp
    tests/test_osm_loader.cpp
    tests/test_cost_matrix.cpp
    tests/test_performance.cpp
    src/quadtree.cpp
    src/assignment.cpp
    src/mcmf.cpp
    src/cost_matrix.cpp
    src/road_graph.cpp
    src/router.cpp
    src/osm_loader.cpp)
target_include_directories(unit_tests PRIVATE src third_party/catch2)

# 6. Register everything with CTest so `ctest` runs it all.
enable_testing()

# Correctness and timing are separate tests on purpose: a red `correctness`
# means the engine is wrong, a red `performance` means it is merely slow. They
# demand different responses, so they should not fail as one.
add_test(NAME correctness COMMAND unit_tests "~[perf]" --order rand --rng-seed time)

if(NOT ENABLE_SANITIZERS)
    add_test(NAME performance COMMAND unit_tests "[perf]")
endif()

# The Week 4 demos double as end-to-end anchors: each exits non-zero if A* and
# Dijkstra ever disagree, or if road-time matching loses on road time.
add_test(NAME routing_end_to_end
         COMMAND route_demo ${CMAKE_CURRENT_SOURCE_DIR}/data/bengaluru_roads.osm 60)
add_test(NAME road_matching_end_to_end
         COMMAND road_match_demo ${CMAKE_CURRENT_SOURCE_DIR}/data/bengaluru_roads.osm 40 40)
