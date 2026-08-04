#include "osm_loader.h"

#include <cmath>
#include <cstdlib>
#include <fstream>
#include <sstream>
#include <stdexcept>
#include <string_view>
#include <unordered_map>
#include <unordered_set>
#include <vector>

namespace {

constexpr double kKmhToMps = 1.0 / 3.6;
constexpr double kMphToKmh = 1.609344;

// Free-flow speeds by highway class, km/h. These are the "no traffic data yet"
// defaults; Week 21's traffic blasting is where they get replaced by measured
// speeds per time-of-day. Conservative on purpose -- an over-estimated speed
// produces an ETA the rider watches tick past.
const std::unordered_map<std::string, double>& defaultSpeedsKmh() {
    static const std::unordered_map<std::string, double> table = {
        {"motorway", 90},       {"motorway_link", 45},
        {"trunk", 70},          {"trunk_link", 40},
        {"primary", 55},        {"primary_link", 35},
        {"secondary", 45},      {"secondary_link", 30},
        {"tertiary", 35},       {"tertiary_link", 25},
        {"unclassified", 30},   {"residential", 25},
        {"living_street", 10},  {"service", 15},
    };
    return table;
}

// Value of attribute `key` in an element's attribute text, or empty.
// Handles the five predefined XML entities, which is the entire escaping
// surface an .osm file uses.
std::string attribute(std::string_view element, std::string_view key) {
    std::string needle;
    needle.reserve(key.size() + 2);
    needle += ' ';
    needle += key;
    needle += '=';

    std::size_t pos = element.find(needle);
    if (pos == std::string_view::npos) return {};
    pos += needle.size();
    if (pos >= element.size()) return {};
    const char quote = element[pos];
    if (quote != '"' && quote != '\'') return {};
    ++pos;
    const std::size_t end = element.find(quote, pos);
    if (end == std::string_view::npos) return {};

    std::string raw(element.substr(pos, end - pos));
    if (raw.find('&') == std::string::npos) return raw;

    static const std::pair<const char*, char> entities[] = {
        {"&amp;", '&'}, {"&lt;", '<'}, {"&gt;", '>'},
        {"&quot;", '"'}, {"&apos;", '\''},
    };
    for (const auto& [text, ch] : entities) {
        for (std::size_t p = raw.find(text); p != std::string::npos; p = raw.find(text, p)) {
            raw.replace(p, std::string(text).size(), 1, ch);
            ++p;
        }
    }
    return raw;
}

// A parsed maxspeed tag in m/s, or 0 if the tag is absent or not a plain speed
// (OSM also carries things like "IN:urban" and "walk", which we ignore rather
// than guess at).
double parseMaxSpeedMps(const std::string& value) {
    if (value.empty()) return 0.0;
    char* end = nullptr;
    const double magnitude = std::strtod(value.c_str(), &end);
    if (end == value.c_str() || magnitude <= 0.0) return 0.0;

    std::string_view unit(end);
    while (!unit.empty() && unit.front() == ' ') unit.remove_prefix(1);
    if (unit.rfind("mph", 0) == 0) return magnitude * kMphToKmh * kKmhToMps;
    if (unit.empty() || unit.rfind("km/h", 0) == 0 || unit.rfind("kmh", 0) == 0) {
        return magnitude * kKmhToMps;
    }
    return 0.0;
}

// oneway direction: +1 forward-only, -1 backward-only, 0 both ways.
int onewayDirection(const std::string& oneway, const std::string& junction) {
    if (oneway == "yes" || oneway == "true" || oneway == "1") return 1;
    if (oneway == "-1" || oneway == "reverse") return -1;
    if (oneway == "no" || oneway == "false" || oneway == "0") return 0;
    // Roundabouts are one-way by definition and are very often left untagged.
    // Getting this wrong lets the router drive the wrong way round every
    // circle in the city -- a route that is fast, optimal, and illegal.
    if (junction == "roundabout" || junction == "circular") return 1;
    return 0;
}

struct PendingWay {
    std::vector<long long> refs;
    std::string highway;
    std::string oneway;
    std::string junction;
    std::string maxspeed;
};

} // namespace

double defaultSpeedMpsForHighway(const std::string& highwayClass) {
    const auto& table = defaultSpeedsKmh();
    auto it = table.find(highwayClass);
    if (it == table.end()) return 0.0;   // unknown class == not drivable
    return it->second * kKmhToMps;
}

RoadGraph loadOsm(const std::string& path) {
    std::ifstream file(path, std::ios::binary);
    if (!file) {
        throw std::runtime_error("loadOsm: cannot open " + path);
    }
    std::ostringstream buffer;
    buffer << file.rdbuf();
    const std::string xml = buffer.str();

    std::unordered_map<long long, std::pair<double, double>> nodeCoords;  // id -> (lat, lon)
    std::vector<PendingWay> ways;
    PendingWay current;
    bool inWay = false;

    // Single forward scan. Every element we care about is a start tag whose
    // attributes sit between '<' and the next '>', so we never need a DOM --
    // which matters when the input is tens of megabytes.
    for (std::size_t i = xml.find('<'); i != std::string::npos; i = xml.find('<', i)) {
        const std::size_t close = xml.find('>', i);
        if (close == std::string::npos) break;
        std::string_view element(xml.data() + i, close - i + 1);
        i = close + 1;

        if (element.rfind("<node", 0) == 0) {
            const std::string id = attribute(element, "id");
            const std::string lat = attribute(element, "lat");
            const std::string lon = attribute(element, "lon");
            if (!id.empty() && !lat.empty() && !lon.empty()) {
                nodeCoords[std::strtoll(id.c_str(), nullptr, 10)] = {std::strtod(lat.c_str(), nullptr),
                                                                     std::strtod(lon.c_str(), nullptr)};
            }
        } else if (element.rfind("<way", 0) == 0) {
            current = PendingWay{};
            // A self-closing <way/> has no members and is of no use to us.
            inWay = element.size() < 2 || element[element.size() - 2] != '/';
        } else if (inWay && element.rfind("<nd", 0) == 0) {
            const std::string ref = attribute(element, "ref");
            if (!ref.empty()) current.refs.push_back(std::strtoll(ref.c_str(), nullptr, 10));
        } else if (inWay && element.rfind("<tag", 0) == 0) {
            const std::string key = attribute(element, "k");
            if (key == "highway")       current.highway  = attribute(element, "v");
            else if (key == "oneway")   current.oneway   = attribute(element, "v");
            else if (key == "junction") current.junction = attribute(element, "v");
            else if (key == "maxspeed") current.maxspeed = attribute(element, "v");
        } else if (element.rfind("</way", 0) == 0) {
            if (inWay && current.refs.size() >= 2 && !current.highway.empty()) {
                ways.push_back(std::move(current));
            }
            inWay = false;
        }
    }

    RoadGraphBuilder builder;
    for (const PendingWay& way : ways) {
        double speed = parseMaxSpeedMps(way.maxspeed);
        if (speed <= 0.0) speed = defaultSpeedMpsForHighway(way.highway);
        if (speed <= 0.0) continue;      // highway class we do not route cars on

        const int direction = onewayDirection(way.oneway, way.junction);

        for (std::size_t k = 0; k + 1 < way.refs.size(); ++k) {
            auto a = nodeCoords.find(way.refs[k]);
            auto b = nodeCoords.find(way.refs[k + 1]);
            // A ref with no node is a way clipped by the extract boundary --
            // expected at the edges, and simply not an arc we can build.
            if (a == nodeCoords.end() || b == nodeCoords.end()) continue;

            const int u = builder.addNode(a->first, a->second.first, a->second.second);
            const int v = builder.addNode(b->first, b->second.first, b->second.second);
            const double length = haversineMeters(a->second.first, a->second.second,
                                                  b->second.first, b->second.second);
            if (length <= 0.0) continue;

            if (direction >= 0) builder.addArc(u, v, length, speed);
            if (direction <= 0) builder.addArc(v, u, length, speed);
        }
    }

    if (builder.numNodes() == 0) {
        throw std::runtime_error("loadOsm: " + path + " contained no drivable ways");
    }
    return RoadGraph(builder);
}

RoadGraph buildGridGraph(int rows, int cols, double spacingMeters,
                         double originLat, double originLon, double speedMps) {
    if (rows < 1 || cols < 1) {
        throw std::invalid_argument("buildGridGraph: rows and cols must be >= 1");
    }

    // Metres -> degrees. Latitude is uniform; longitude shrinks by cos(lat),
    // which is why a grid built naively looks square on paper and is not.
    constexpr double kMetersPerDegreeLat = 111320.0;
    const double dLat = spacingMeters / kMetersPerDegreeLat;
    const double dLon = spacingMeters /
                        (kMetersPerDegreeLat * std::cos(originLat * 3.14159265358979323846 / 180.0));

    RoadGraphBuilder builder;
    auto coord = [&](int r, int c) {
        return std::pair<double, double>{originLat + r * dLat, originLon + c * dLon};
    };

    // Row-major: index[r * cols + c] is the graph index of grid cell (r, c).
    std::vector<int> index(static_cast<std::size_t>(rows) * cols);
    for (int r = 0; r < rows; ++r) {
        for (int c = 0; c < cols; ++c) {
            auto [lat, lon] = coord(r, c);
            index[static_cast<std::size_t>(r) * cols + c] =
                builder.addNode(static_cast<long long>(r) * cols + c + 1, lat, lon);
        }
    }

    auto link = [&](int r1, int c1, int r2, int c2) {
        auto [lat1, lon1] = coord(r1, c1);
        auto [lat2, lon2] = coord(r2, c2);
        const double len = haversineMeters(lat1, lon1, lat2, lon2);
        const int u = index[static_cast<std::size_t>(r1) * cols + c1];
        const int v = index[static_cast<std::size_t>(r2) * cols + c2];
        builder.addArc(u, v, len, speedMps);
        builder.addArc(v, u, len, speedMps);
    };

    for (int r = 0; r < rows; ++r) {
        for (int c = 0; c < cols; ++c) {
            if (c + 1 < cols) link(r, c, r, c + 1);
            if (r + 1 < rows) link(r, c, r + 1, c);
        }
    }
    return RoadGraph(builder);
}
