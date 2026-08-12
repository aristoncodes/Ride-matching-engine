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

# 4b. Week 6: the gRPC server.
#
# gRPC is looked up rather than assumed: the algorithmic targets above must
# still build on a machine that has never installed it, because the engine's
# correctness does not depend on how it is transported. Only the server and its
# integration test are skipped when gRPC is missing.
find_package(Protobuf CONFIG QUIET)
find_package(gRPC CONFIG QUIET)

if(gRPC_FOUND AND Protobuf_FOUND)
    set(GENERATED_DIR ${CMAKE_CURRENT_SOURCE_DIR}/gen)
    set(GENERATED_SOURCES
        ${GENERATED_DIR}/matching.pb.cc
        ${GENERATED_DIR}/matching.grpc.pb.cc)

    # Regenerate whenever the .proto is newer. `make proto` at the repo root
    # does the same thing for both languages; this keeps a plain `cmake --build`
    # from silently compiling stubs that no longer match the contract.
    find_program(PROTOC_EXECUTABLE protoc REQUIRED)
    find_program(GRPC_CPP_PLUGIN grpc_cpp_plugin REQUIRED)
    set(PROTO_FILE ${CMAKE_CURRENT_SOURCE_DIR}/../docs/api/matching.proto)
    add_custom_command(
        OUTPUT ${GENERATED_SOURCES}
               ${GENERATED_DIR}/matching.pb.h ${GENERATED_DIR}/matching.grpc.pb.h
        COMMAND ${CMAKE_COMMAND} -E make_directory ${GENERATED_DIR}
        COMMAND ${PROTOC_EXECUTABLE}
                -I ${CMAKE_CURRENT_SOURCE_DIR}/../docs/api
                --cpp_out=${GENERATED_DIR}
                --grpc_out=${GENERATED_DIR}
                --plugin=protoc-gen-grpc=${GRPC_CPP_PLUGIN}
                ${PROTO_FILE}
        DEPENDS ${PROTO_FILE}
        COMMENT "Generating C++ gRPC stubs from matching.proto")

    add_library(matching_service STATIC
        src/matching_service.cpp src/graph_registry.cpp
        src/assignment.cpp src/mcmf.cpp src/cost_matrix.cpp
        src/quadtree.cpp src/road_graph.cpp src/router.cpp src/osm_loader.cpp
        ${GENERATED_SOURCES})
    target_include_directories(matching_service PUBLIC src ${GENERATED_DIR})
    target_link_libraries(matching_service PUBLIC gRPC::grpc++ gRPC::grpc++_reflection
                                                  protobuf::libprotobuf)

    add_executable(matching_server src/matching_server.cpp)
    target_link_libraries(matching_server PRIVATE matching_service)

    add_executable(service_test tests/test_main.cpp tests/test_matching_service.cpp)
    target_include_directories(service_test PRIVATE src third_party/catch2 ${GENERATED_DIR})
    target_link_libraries(service_test PRIVATE matching_service)
else()
    message(STATUS "gRPC not found — skipping matching_server. "
                   "Install with: brew install grpc protobuf")
endif()

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

# Week 6: the gRPC service's contract semantics (error codes, unmatched
# reasons, which fields are set under which metric). Registered only when gRPC
# was found, so a machine without it still gets a green `ctest` for the engine.
if(gRPC_FOUND AND Protobuf_FOUND)
    add_test(NAME service_contract COMMAND service_test)
endif()
