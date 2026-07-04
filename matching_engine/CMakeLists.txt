# 1. Always set the minimum required version first
cmake_minimum_required(VERSION 3.10)

# 2. Then call the project() command
project(RideMatchingEngine)

# 3. Then set your standards
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# 4. Executables
#    generator  — Week 1: standalone random coordinate generator (no deps)
#    match_demo — interim greedy nearest-neighbor demo exercising the QuadTree
add_executable(generator src/generator.cpp)
add_executable(match_demo src/match_demo.cpp src/quadtree.cpp)
