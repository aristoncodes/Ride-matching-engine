// Week 1 deliverable: a configurable, reproducible generator of random rider
// and driver coordinates on a square grid.
//
// Definition of done:
//   - Parameterized via CLI (riders, drivers, grid size, seed, format).
//   - Reproducible: a fixed --seed produces byte-identical output.
//   - Machine-readable output (csv / json) in addition to human-readable text.
//   - Unambiguous, type-prefixed IDs (R0.., D0..) so a combined set is never
//     ambiguous (see docs/Data_Model.md).
//
// Output goes to stdout (pipeable / redirectable); the seed actually used and
// the run parameters are reported on stderr so stdout stays clean data.

#include <iostream>
#include <random>
#include <string>
#include <cstdint>
#include <cstdlib>
#include <iomanip>

namespace {

struct Config {
    long          riders  = 10000;
    long          drivers = 10000;
    double        grid    = 100000.0;
    std::uint32_t seed    = 0;
    bool          seedProvided = false;
    std::string   format  = "text"; // text | csv | json
};

void printUsage(const char* prog) {
    std::cerr <<
        "Usage: " << prog << " [options]\n"
        "Generate random rider and driver coordinates on a square grid.\n\n"
        "Options:\n"
        "  -N, --riders <int>    number of riders    (default 10000)\n"
        "  -M, --drivers <int>   number of drivers   (default 10000)\n"
        "  -g, --grid <float>    grid side length    (default 100000)\n"
        "  -s, --seed <uint>     RNG seed for reproducible output\n"
        "                        (default: random, non-reproducible)\n"
        "  -f, --format <fmt>    output: text | csv | json  (default text)\n"
        "  -h, --help            show this help\n\n"
        "Coordinates are drawn from [0, grid) on each axis. With the same --seed\n"
        "the coordinates are identical across all formats.\n";
}

} // namespace

int main(int argc, char** argv) {
    Config cfg;

    for (int i = 1; i < argc; ++i) {
        std::string a = argv[i];
        auto value = [&](const char* name) -> std::string {
            if (i + 1 >= argc) {
                std::cerr << "Error: " << name << " requires a value\n";
                std::exit(2);
            }
            return argv[++i];
        };

        if      (a == "-N" || a == "--riders")  cfg.riders  = std::stol(value("--riders"));
        else if (a == "-M" || a == "--drivers") cfg.drivers = std::stol(value("--drivers"));
        else if (a == "-g" || a == "--grid")    cfg.grid    = std::stod(value("--grid"));
        else if (a == "-s" || a == "--seed")  { cfg.seed = static_cast<std::uint32_t>(std::stoul(value("--seed")));
                                                cfg.seedProvided = true; }
        else if (a == "-f" || a == "--format")  cfg.format  = value("--format");
        else if (a == "-h" || a == "--help")  { printUsage(argv[0]); return 0; }
        else { std::cerr << "Error: unknown option '" << a << "'\n\n"; printUsage(argv[0]); return 2; }
    }

    // Validation
    if (cfg.riders < 0 || cfg.drivers < 0) { std::cerr << "Error: counts must be >= 0\n"; return 2; }
    if (cfg.grid <= 0.0)                   { std::cerr << "Error: --grid must be > 0\n"; return 2; }
    if (cfg.format != "text" && cfg.format != "csv" && cfg.format != "json") {
        std::cerr << "Error: --format must be text, csv, or json\n"; return 2;
    }

    // Seed: use the provided one, or draw a random one (and report it either way
    // so a "random" run can still be reproduced later).
    std::uint32_t seed = cfg.seed;
    if (!cfg.seedProvided) {
        std::random_device rd;
        seed = rd();
    }
    std::mt19937 gen(seed);
    std::uniform_real_distribution<double> dis(0.0, cfg.grid);

    std::cerr << "generator: seed=" << seed
              << " riders=" << cfg.riders
              << " drivers=" << cfg.drivers
              << " grid=" << cfg.grid
              << " format=" << cfg.format << "\n";

    std::cout << std::setprecision(6);

    // Emit riders first, then drivers. The number and order of RNG draws is
    // fixed (2 per entity), so identical seeds yield identical coordinates
    // regardless of the chosen output format.
    if (cfg.format == "csv") {
        std::cout << "type,id,x,y\n";
        for (long i = 0; i < cfg.riders; ++i) {
            double x = dis(gen), y = dis(gen);
            std::cout << "rider,R" << i << ',' << x << ',' << y << '\n';
        }
        for (long i = 0; i < cfg.drivers; ++i) {
            double x = dis(gen), y = dis(gen);
            std::cout << "driver,D" << i << ',' << x << ',' << y << '\n';
        }
    } else if (cfg.format == "json") {
        std::cout << "{\n  \"riders\": [\n";
        for (long i = 0; i < cfg.riders; ++i) {
            double x = dis(gen), y = dis(gen);
            std::cout << "    {\"id\": \"R" << i << "\", \"x\": " << x << ", \"y\": " << y << "}"
                      << (i + 1 < cfg.riders ? "," : "") << '\n';
        }
        std::cout << "  ],\n  \"drivers\": [\n";
        for (long i = 0; i < cfg.drivers; ++i) {
            double x = dis(gen), y = dis(gen);
            std::cout << "    {\"id\": \"D" << i << "\", \"x\": " << x << ", \"y\": " << y << "}"
                      << (i + 1 < cfg.drivers ? "," : "") << '\n';
        }
        std::cout << "  ]\n}\n";
    } else { // text
        for (long i = 0; i < cfg.riders; ++i) {
            double x = dis(gen), y = dis(gen);
            std::cout << "Rider R" << i << ": (" << x << ", " << y << ")\n";
        }
        for (long i = 0; i < cfg.drivers; ++i) {
            double x = dis(gen), y = dis(gen);
            std::cout << "Driver D" << i << ": (" << x << ", " << y << ")\n";
        }
    }

    return 0;
}
