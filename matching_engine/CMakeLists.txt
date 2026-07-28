# 1. Always set the minimum required version first
cmake_minimum_required(VERSION 3.10)

# 2. Then call the project() command
project(RideMatchingEngine)

# 3. Then set your standards
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# 4. Executables
#    generator       — Week 1: standalone random coordinate generator (no deps)
#    match_demo      — interim greedy nearest-neighbor demo exercising the QuadTree
#    benchmark       — Week 2 stretch: quadtree vs brute-force nearest-neighbor timing
#    assignment_test — Week 3: MCMF assignment solver vs brute-force optimality oracle
#    optimal_match   — Week 3 showcase: greedy vs optimal (dense/sparse), overflow
add_executable(generator src/generator.cpp)
add_executable(match_demo src/match_demo.cpp src/quadtree.cpp)
add_executable(benchmark src/benchmark.cpp src/quadtree.cpp)
add_executable(assignment_test src/assignment_test.cpp src/assignment.cpp src/mcmf.cpp)
add_executable(optimal_match src/optimal_match.cpp src/assignment.cpp src/mcmf.cpp
               src/cost_matrix.cpp src/quadtree.cpp)

# 5. Register the correctness anchors with CTest so `ctest` runs them.
enable_testing()
add_test(NAME assignment_optimality COMMAND assignment_test)
