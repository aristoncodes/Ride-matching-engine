#ifndef OSM_LOADER_H
#define OSM_LOADER_H

#include <string>

#include "road_graph.h"

// Turn an OpenStreetMap .osm XML extract into a routable RoadGraph.
//
// Deliberately hand-rolled instead of pulling in libosmium/expat: the subset of
// XML an .osm file uses is tiny (three element types, no namespaces, no CDATA,
// no entities beyond the five predefined ones), and a dependency-free loader
// keeps `cmake && make` working on any machine. It is a parser for OUR input,
// not a general XML parser, and it says so.
//
// What it understands:
//   <node id lat lon>            -> a junction / shape point
//   <way> <nd ref>... <tag k v>  -> a road, if tagged with a drivable highway
//   oneway=yes|true|1|-1|reverse -> arc direction (-1 means digitised backwards)
//   junction=roundabout          -> implicitly one-way
//   maxspeed=NN [mph]            -> speed, else a default for the highway class
//
// Consecutive node pairs in a way become arcs. Speed is metres/second; edge
// weight is metres/speed = SECONDS.
RoadGraph loadOsm(const std::string& path);

// Speed in m/s used when a way carries no usable maxspeed tag. Exposed for
// tests: an untagged residential street must not silently become a motorway.
double defaultSpeedMpsForHighway(const std::string& highwayClass);

// A deterministic rows x cols grid of streets, spacing metres apart, centred on
// (originLat, originLon). Every arc is bidirectional at a uniform speed.
//
// This exists so the test suite never depends on a multi-megabyte data file or
// a network fetch: on a grid the true shortest path is known analytically, so
// the router can be checked against arithmetic instead of against itself.
RoadGraph buildGridGraph(int rows, int cols, double spacingMeters,
                         double originLat = 12.9700, double originLon = 77.5900,
                         double speedMps = 10.0);

#endif // OSM_LOADER_H
