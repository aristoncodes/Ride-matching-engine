// OSM loader: does the graph we build actually reflect what the map says?
//
// Every test here writes a small .osm file and reads it back, because the
// failure modes are all about interpretation, not arithmetic: a one-way read
// as two-way lets the router send drivers the wrong way up a street, and a
// missing speed default silently turns a lane into a motorway.

#include "catch.hpp"

#include <cstdio>
#include <fstream>
#include <string>

#include "osm_loader.h"
#include "road_graph.h"
#include "router.h"

namespace {

// Writes `content` to a scratch file and removes it when the test ends,
// whether it passed, failed, or threw.
class TempOsmFile {
public:
    explicit TempOsmFile(const std::string& content) {
        static int counter = 0;
        path_ = "test_osm_" + std::to_string(counter++) + ".osm";
        std::ofstream out(path_);
        out << content;
    }
    ~TempOsmFile() { std::remove(path_.c_str()); }
    TempOsmFile(const TempOsmFile&) = delete;
    TempOsmFile& operator=(const TempOsmFile&) = delete;

    const std::string& path() const { return path_; }

private:
    std::string path_;
};

// Three nodes in a north-south line, roughly 111 m apart, joined by one way.
std::string threeNodeWay(const std::string& tags) {
    return R"(<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6">
  <node id="1" lat="12.9700" lon="77.5900"/>
  <node id="2" lat="12.9710" lon="77.5900"/>
  <node id="3" lat="12.9720" lon="77.5900"/>
  <way id="100">
    <nd ref="1"/><nd ref="2"/><nd ref="3"/>
)" + tags + R"(
  </way>
</osm>
)";
}

} // namespace

TEST_CASE("a plain two-way street becomes arcs in both directions", "[osm]") {
    const TempOsmFile file(threeNodeWay(R"(<tag k="highway" v="residential"/>)"));
    const RoadGraph graph = loadOsm(file.path());

    REQUIRE(graph.numNodes() == 3);
    REQUIRE(graph.numArcs() == 4);          // 2 segments x 2 directions

    // Node ids survive, which is what makes a route traceable back to the map.
    std::vector<long long> osmIds;
    for (int v = 0; v < graph.numNodes(); ++v) osmIds.push_back(graph.node(v).osmId);
    std::sort(osmIds.begin(), osmIds.end());
    REQUIRE(osmIds == std::vector<long long>{1, 2, 3});

    REQUIRE(dijkstra(graph, 0, 2).found());
    REQUIRE(dijkstra(graph, 2, 0).found());
    REQUIRE(graph.largestComponent().size() == 3);
}

TEST_CASE("oneway=yes makes the street traversable in one direction only", "[osm]") {
    const TempOsmFile file(threeNodeWay(
        R"(<tag k="highway" v="residential"/><tag k="oneway" v="yes"/>)"));
    const RoadGraph graph = loadOsm(file.path());

    REQUIRE(graph.numArcs() == 2);          // 2 segments, forward only
    REQUIRE(dijkstra(graph, 0, 2).found());
    REQUIRE_FALSE(dijkstra(graph, 2, 0).found());
}

TEST_CASE("oneway=-1 means the way is digitised backwards", "[osm][edge]") {
    // A genuinely easy tag to get wrong, and the consequence is drivers routed
    // the wrong way down a street that the map got right.
    const TempOsmFile file(threeNodeWay(
        R"(<tag k="highway" v="residential"/><tag k="oneway" v="-1"/>)"));
    const RoadGraph graph = loadOsm(file.path());

    REQUIRE(graph.numArcs() == 2);
    const int first = graph.node(0).osmId == 1 ? 0 : 2;
    const int last = graph.node(0).osmId == 1 ? 2 : 0;
    REQUIRE_FALSE(dijkstra(graph, first, last).found());
    REQUIRE(dijkstra(graph, last, first).found());
}

TEST_CASE("oneway=no is explicitly two-way", "[osm][edge]") {
    const TempOsmFile file(threeNodeWay(
        R"(<tag k="highway" v="residential"/><tag k="oneway" v="no"/>)"));
    REQUIRE(loadOsm(file.path()).numArcs() == 4);
}

TEST_CASE("an untagged roundabout is still one-way", "[osm][edge]") {
    const TempOsmFile file(threeNodeWay(
        R"(<tag k="highway" v="residential"/><tag k="junction" v="roundabout"/>)"));
    REQUIRE(loadOsm(file.path()).numArcs() == 2);
}

TEST_CASE("maxspeed is parsed, in km/h and in mph", "[osm]") {
    const TempOsmFile metric(threeNodeWay(
        R"(<tag k="highway" v="residential"/><tag k="maxspeed" v="50"/>)"));
    REQUIRE(loadOsm(metric.path()).maxSpeedMps() == Approx(50.0 / 3.6));

    const TempOsmFile imperial(threeNodeWay(
        R"(<tag k="highway" v="residential"/><tag k="maxspeed" v="30 mph"/>)"));
    REQUIRE(loadOsm(imperial.path()).maxSpeedMps() == Approx(30.0 * 1.609344 / 3.6));

    // Values OSM carries that are not plain speeds fall back to the class
    // default rather than being guessed at.
    const TempOsmFile coded(threeNodeWay(
        R"(<tag k="highway" v="residential"/><tag k="maxspeed" v="IN:urban"/>)"));
    REQUIRE(loadOsm(coded.path()).maxSpeedMps() ==
            Approx(defaultSpeedMpsForHighway("residential")));
}

TEST_CASE("an untagged way gets its highway class default, not a motorway", "[osm]") {
    REQUIRE(defaultSpeedMpsForHighway("motorway") > defaultSpeedMpsForHighway("residential"));
    REQUIRE(defaultSpeedMpsForHighway("residential") >
            defaultSpeedMpsForHighway("living_street"));
    // An unknown class is not drivable, and must not silently inherit anything.
    REQUIRE(defaultSpeedMpsForHighway("footway") == Approx(0.0));
    REQUIRE(defaultSpeedMpsForHighway("") == Approx(0.0));

    const TempOsmFile file(threeNodeWay(R"(<tag k="highway" v="motorway"/>)"));
    REQUIRE(loadOsm(file.path()).maxSpeedMps() == Approx(defaultSpeedMpsForHighway("motorway")));
}

TEST_CASE("ways a car cannot use are excluded", "[osm][edge]") {
    // A footpath is a perfectly good OSM highway and a terrible driving route.
    const std::string xml = R"(<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6">
  <node id="1" lat="12.9700" lon="77.5900"/>
  <node id="2" lat="12.9710" lon="77.5900"/>
  <node id="3" lat="12.9720" lon="77.5900"/>
  <node id="4" lat="12.9730" lon="77.5900"/>
  <way id="100"><nd ref="1"/><nd ref="2"/><tag k="highway" v="residential"/></way>
  <way id="101"><nd ref="3"/><nd ref="4"/><tag k="highway" v="footway"/></way>
  <way id="102"><nd ref="3"/><nd ref="4"/><tag k="highway" v="steps"/></way>
</osm>
)";
    const TempOsmFile file(xml);
    const RoadGraph graph = loadOsm(file.path());

    // Only the residential way survives, so only its two nodes exist.
    REQUIRE(graph.numNodes() == 2);
    REQUIRE(graph.numArcs() == 2);
}

TEST_CASE("node references outside the extract are skipped, not fatal", "[osm][edge]") {
    // Every real extract has these: a road clipped at the bounding box keeps
    // referencing nodes that were never downloaded.
    const std::string xml = R"(<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6">
  <node id="1" lat="12.9700" lon="77.5900"/>
  <node id="2" lat="12.9710" lon="77.5900"/>
  <way id="100">
    <nd ref="1"/><nd ref="2"/><nd ref="999999"/>
    <tag k="highway" v="primary"/>
  </way>
</osm>
)";
    const TempOsmFile file(xml);
    const RoadGraph graph = loadOsm(file.path());
    REQUIRE(graph.numNodes() == 2);
    REQUIRE(graph.numArcs() == 2);      // the 2 -> 999999 segment simply is not built
}

TEST_CASE("shared nodes stitch separate ways into one network", "[osm]") {
    // This is the whole reason node ids are de-duplicated: two ways that meet
    // at a junction must produce ONE node, or the network is a pile of
    // disconnected sticks and nothing is reachable.
    const std::string xml = R"(<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6">
  <node id="1" lat="12.9700" lon="77.5900"/>
  <node id="2" lat="12.9710" lon="77.5900"/>
  <node id="3" lat="12.9710" lon="77.5910"/>
  <way id="100"><nd ref="1"/><nd ref="2"/><tag k="highway" v="residential"/></way>
  <way id="101"><nd ref="2"/><nd ref="3"/><tag k="highway" v="residential"/></way>
</osm>
)";
    const TempOsmFile file(xml);
    const RoadGraph graph = loadOsm(file.path());

    REQUIRE(graph.numNodes() == 3);
    REQUIRE(graph.largestComponent().size() == 3);
    // Node 1 to node 3 crosses the join, so it only works if the join is real.
    REQUIRE(dijkstra(graph, 0, 2).found());
}

TEST_CASE("a file with no drivable ways is an error, not an empty graph", "[osm][edge]") {
    const std::string xml = R"(<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6">
  <node id="1" lat="12.9700" lon="77.5900"/>
</osm>
)";
    const TempOsmFile file(xml);
    // Silently returning an empty graph would turn a bad data path into an
    // engine that matches nobody and says nothing.
    REQUIRE_THROWS_AS(loadOsm(file.path()), std::runtime_error);
}

TEST_CASE("a missing file is reported clearly", "[osm][edge]") {
    REQUIRE_THROWS_AS(loadOsm("definitely_not_a_real_file.osm"), std::runtime_error);
}

TEST_CASE("a way with a single node produces no arcs", "[osm][edge]") {
    const std::string xml = R"(<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6">
  <node id="1" lat="12.9700" lon="77.5900"/>
  <node id="2" lat="12.9710" lon="77.5900"/>
  <node id="3" lat="12.9720" lon="77.5900"/>
  <way id="100"><nd ref="1"/><tag k="highway" v="residential"/></way>
  <way id="101"><nd ref="2"/><nd ref="3"/><tag k="highway" v="residential"/></way>
</osm>
)";
    const TempOsmFile file(xml);
    const RoadGraph graph = loadOsm(file.path());
    REQUIRE(graph.numNodes() == 2);
    REQUIRE(graph.numArcs() == 2);
}

TEST_CASE("attribute values with XML entities are decoded", "[osm][edge]") {
    // Road names in India regularly contain '&'. If the decoder mangles an
    // attribute it can just as easily mangle a coordinate.
    const std::string xml = R"(<?xml version="1.0" encoding="UTF-8"?>
<osm version="0.6">
  <node id="1" lat="12.9700" lon="77.5900"/>
  <node id="2" lat="12.9710" lon="77.5900"/>
  <way id="100">
    <nd ref="1"/><nd ref="2"/>
    <tag k="highway" v="residential"/>
    <tag k="name" v="Brigade &amp; Church St &lt;east&gt;"/>
  </way>
</osm>
)";
    const TempOsmFile file(xml);
    const RoadGraph graph = loadOsm(file.path());
    REQUIRE(graph.numNodes() == 2);
    REQUIRE(graph.node(0).lat == Approx(12.9700));
}

TEST_CASE("the synthetic grid is a usable stand-in for a real extract", "[osm]") {
    const RoadGraph grid = buildGridGraph(5, 4, 100.0);
    REQUIRE(grid.numNodes() == 20);
    // 5 rows x 3 horizontal links + 4 rows-of-links x 4 columns = 15 + 16 = 31
    // undirected edges, each stored as two arcs.
    REQUIRE(grid.numArcs() == 2 * (5 * 3 + 4 * 4));
    REQUIRE(grid.largestComponent().size() == 20);

    REQUIRE_THROWS_AS(buildGridGraph(0, 5, 100.0), std::invalid_argument);
    REQUIRE_THROWS_AS(buildGridGraph(5, 0, 100.0), std::invalid_argument);

    // A 1x1 grid has a node and no roads -- degenerate, but not a crash.
    const RoadGraph single = buildGridGraph(1, 1, 100.0);
    REQUIRE(single.numNodes() == 1);
    REQUIRE(single.numArcs() == 0);
}
