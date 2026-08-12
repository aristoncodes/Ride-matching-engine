#include "graph_registry.h"

#include <algorithm>
#include <stdexcept>

#include "osm_loader.h"

void GraphRegistry::load(const std::string& id, const std::string& path) {
    if (graphs_.count(id)) {
        throw std::invalid_argument("GraphRegistry: duplicate graph id '" + id + "'");
    }
    graphs_.emplace(id, std::make_unique<RoadGraph>(loadOsm(path)));
}

void GraphRegistry::add(const std::string& id, std::unique_ptr<RoadGraph> graph) {
    if (!graph) throw std::invalid_argument("GraphRegistry: null graph for '" + id + "'");
    graphs_[id] = std::move(graph);
}

const RoadGraph* GraphRegistry::find(const std::string& id) const {
    auto it = graphs_.find(id);
    return it == graphs_.end() ? nullptr : it->second.get();
}

std::vector<std::string> GraphRegistry::ids() const {
    std::vector<std::string> out;
    out.reserve(graphs_.size());
    for (const auto& [id, _] : graphs_) out.push_back(id);
    std::sort(out.begin(), out.end());
    return out;
}
