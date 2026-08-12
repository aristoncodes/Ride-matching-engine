// The C++ matching engine as a standalone gRPC server.
//
// Standalone is the whole point (ADR-0002): running as a separate process means
// a segfault in this code returns an UNAVAILABLE to Go instead of taking the Go
// ingestion layer down with it. The batch is then simply never acked, and gets
// redelivered. That failure isolation is not achievable with cgo, which welds
// the two lifetimes together.
//
// Usage:
//   matching_server [--port 50051] [--graph <id>=<path>]... [--max-riders N]
//
// Example:
//   matching_server --port 50051 --graph blr-central=../data/bengaluru_roads.osm

#include <grpcpp/ext/proto_server_reflection_plugin.h>
#include <grpcpp/grpcpp.h>
#include <grpcpp/health_check_service_interface.h>

#include <atomic>
#include <chrono>
#include <csignal>
#include <cstdlib>
#include <iostream>
#include <memory>
#include <string>
#include <thread>

#include "graph_registry.h"
#include "matching_service.h"

namespace {

// Set from a signal handler, so it must be exactly this type: a plain bool is
// not guaranteed to be readable without tearing, and anything more complex
// (locking, allocating, logging) is undefined behaviour inside a handler.
std::atomic<bool> g_shutdownRequested{false};

void handleSignal(int) { g_shutdownRequested.store(true); }

[[noreturn]] void usage(const char* argv0, const std::string& problem) {
    std::cerr << "error: " << problem << "\n\n"
              << "usage: " << argv0 << " [options]\n"
              << "  --port <n>            listen port (default 50051)\n"
              << "  --graph <id>=<path>   load an .osm extract under an id "
                 "(repeatable)\n"
              << "  --max-riders <n>      per-batch rider limit (default 5000)\n"
              << "  --max-drivers <n>     per-batch driver limit (default 5000)\n";
    std::exit(2);
}

} // namespace

int main(int argc, char** argv) {
    int port = 50051;
    MatchingService::Options options;
    options.version = "week6-dev";
    GraphRegistry graphs;

    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        auto next = [&](const char* what) -> std::string {
            if (i + 1 >= argc) usage(argv[0], std::string(what) + " needs a value");
            return argv[++i];
        };

        if (arg == "--port") {
            port = std::atoi(next("--port").c_str());
            // 0 is legal and useful: it asks the OS for any free port, which
            // the server then prints. Tests rely on it so they can run in
            // parallel without fighting over a hardcoded port.
            if (port < 0 || port > 65535) usage(argv[0], "--port out of range");
        } else if (arg == "--graph") {
            const std::string spec = next("--graph");
            const std::size_t eq = spec.find('=');
            if (eq == std::string::npos) usage(argv[0], "--graph expects <id>=<path>");
            const std::string id = spec.substr(0, eq);
            const std::string path = spec.substr(eq + 1);
            try {
                const auto started = std::chrono::steady_clock::now();
                graphs.load(id, path);
                const auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(
                                    std::chrono::steady_clock::now() - started).count();
                const RoadGraph* g = graphs.find(id);
                std::cout << "loaded graph '" << id << "' from " << path << " ("
                          << g->numNodes() << " nodes, " << g->numArcs() << " arcs, "
                          << ms << " ms)\n";
            } catch (const std::exception& e) {
                // Fail at STARTUP, loudly. A server that comes up without the
                // graph it was told to load would accept traffic and reject it
                // one request at a time -- an outage disguised as a high error
                // rate, which is far harder to diagnose than a refusal to boot.
                std::cerr << "fatal: could not load graph '" << id << "': " << e.what() << "\n";
                return 1;
            }
        } else if (arg == "--max-riders") {
            options.maxRiders = std::atoi(next("--max-riders").c_str());
        } else if (arg == "--max-drivers") {
            options.maxDrivers = std::atoi(next("--max-drivers").c_str());
        } else if (arg == "--help" || arg == "-h") {
            usage(argv[0], "help requested");
        } else {
            usage(argv[0], "unknown argument: " + arg);
        }
    }

    MatchingService service(graphs, options);

    grpc::EnableDefaultHealthCheckService(true);
    grpc::reflection::InitProtoReflectionServerBuilderPlugin();

    const std::string address = "0.0.0.0:" + std::to_string(port);
    grpc::ServerBuilder builder;
    int boundPort = 0;
    builder.AddListeningPort(address, grpc::InsecureServerCredentials(), &boundPort);
    builder.RegisterService(&service);

    std::unique_ptr<grpc::Server> server(builder.BuildAndStart());
    if (!server || boundPort == 0) {
        std::cerr << "fatal: could not bind " << address << " (port already in use?)\n";
        return 1;
    }

    // Printed on a line of its own and flushed, because the integration tests
    // wait for exactly this before dialing. Racing a fixed sleep against
    // process startup is how test suites become intermittently red.
    std::cout << "matching_server listening on " << boundPort << std::endl;

    std::signal(SIGINT, handleSignal);
    std::signal(SIGTERM, handleSignal);

    // Poll the flag rather than calling Shutdown() from the handler: gRPC's
    // Shutdown allocates and locks, neither of which is legal in a signal
    // handler. The handler does the one thing it safely can -- set a flag.
    std::thread watchdog([&server] {
        while (!g_shutdownRequested.load()) {
            std::this_thread::sleep_for(std::chrono::milliseconds(100));
        }
        std::cout << "shutting down...\n";
        // A deadline, so a request that hangs cannot hold the process open
        // forever. In-flight work gets a grace period, then the door closes.
        server->Shutdown(std::chrono::system_clock::now() + std::chrono::seconds(5));
    });

    server->Wait();
    g_shutdownRequested.store(true);   // release the watchdog if Wait() returned first
    watchdog.join();
    std::cout << "stopped cleanly\n";
    return 0;
}
