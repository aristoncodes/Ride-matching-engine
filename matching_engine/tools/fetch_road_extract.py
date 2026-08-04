#!/usr/bin/env python3
"""Download a real OpenStreetMap extract and strip it down to just the road network.

Why this exists: the C++ router (Week 4) needs a graph whose edges are actual
roads -- with one-ways, speed limits, and the fact that you cannot drive across
a lake. A raw OSM download is mostly things we do not care about (buildings,
shops, park benches), so we filter it here, once, in a throwaway script rather
than paying for it in the hot C++ loader.

The output is a valid .osm XML file containing only:
  - ways tagged highway=<a drivable class>
  - the nodes those ways actually reference

api.openstreetmap.org caps a single bbox request (roughly 50k nodes), so we
fetch a grid of small tiles and merge them. Nodes shared across tile borders
de-duplicate by OSM id, which is what stitches the tiles into one graph.

Usage:
    python3 fetch_road_extract.py --out ../data/bengaluru_roads.osm
"""

import argparse
import shutil
import subprocess
import sys
import time
import urllib.request
import xml.etree.ElementTree as ET

# Highway classes a car can actually drive on. Footways, cycleways and steps are
# deliberately excluded -- routing a driver down a staircase is not a rounding error.
DRIVABLE = {
    "motorway", "trunk", "primary", "secondary", "tertiary",
    "unclassified", "residential", "living_street", "service",
    "motorway_link", "trunk_link", "primary_link", "secondary_link", "tertiary_link",
}

# Tags worth carrying into the graph. Everything else is dropped.
KEEP_TAGS = {"highway", "oneway", "maxspeed", "name", "junction"}

API = "https://api.openstreetmap.org/api/0.6/map?bbox={},{},{},{}"


def fetch_tile(min_lon, min_lat, max_lon, max_lat, retries=3):
    """Fetch one bbox. Prefers curl, which uses the OS trust store -- a stock
    python.org install on macOS ships without CA certificates and every
    urlopen() to https fails with CERTIFICATE_VERIFY_FAILED."""
    url = API.format(min_lon, min_lat, max_lon, max_lat)
    for attempt in range(retries):
        try:
            if shutil.which("curl"):
                done = subprocess.run(["curl", "-sSfL", "--max-time", "180", url],
                                      capture_output=True, timeout=200)
                if done.returncode == 0 and done.stdout:
                    return done.stdout
                raise RuntimeError(done.stderr.decode("utf-8", "replace").strip()
                                   or f"curl exit {done.returncode}")
            with urllib.request.urlopen(url, timeout=180) as resp:
                return resp.read()
        except Exception as exc:  # noqa: BLE001 - a fetch script, not a library
            print(f"  attempt {attempt + 1} failed: {exc}", file=sys.stderr)
            time.sleep(5 * (attempt + 1))
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    # Default area: central Bengaluru (MG Road / Cubbon Park / Richmond Town).
    ap.add_argument("--min-lon", type=float, default=77.575)
    ap.add_argument("--min-lat", type=float, default=12.950)
    ap.add_argument("--max-lon", type=float, default=77.635)
    ap.add_argument("--max-lat", type=float, default=12.996)
    ap.add_argument("--tile", type=float, default=0.015, help="tile side in degrees")
    args = ap.parse_args()

    nodes = {}   # osm id -> (lat, lon)
    ways = {}    # osm id -> (list of node refs, dict of tags)

    lat = args.min_lat
    tiles = 0
    while lat < args.max_lat:
        lon = args.min_lon
        while lon < args.max_lon:
            hi_lon = min(lon + args.tile, args.max_lon)
            hi_lat = min(lat + args.tile, args.max_lat)
            # Skip slivers left over by a bbox that is not a whole number of
            # tiles wide -- a request for a 100m strip costs the same as a real tile.
            if (hi_lon - lon) < args.tile * 0.2 or (hi_lat - lat) < args.tile * 0.2:
                lon = hi_lon
                continue
            tiles += 1
            print(f"tile {tiles}: {lon:.4f},{lat:.4f} -> {hi_lon:.4f},{hi_lat:.4f}")
            raw = fetch_tile(lon, lat, hi_lon, hi_lat)
            if raw is None:
                print("  giving up on this tile", file=sys.stderr)
                lon = hi_lon
                continue

            root = ET.fromstring(raw)
            for el in root:
                if el.tag == "node":
                    nodes[el.get("id")] = (el.get("lat"), el.get("lon"))
                elif el.tag == "way":
                    tags = {t.get("k"): t.get("v") for t in el.findall("tag")}
                    if tags.get("highway") not in DRIVABLE:
                        continue
                    refs = [nd.get("ref") for nd in el.findall("nd")]
                    if len(refs) < 2:
                        continue
                    ways[el.get("id")] = (refs, {k: v for k, v in tags.items() if k in KEEP_TAGS})
            lon = hi_lon
            time.sleep(1)  # be polite to the API
        lat += args.tile

    # Keep only nodes that some road actually references.
    referenced = {r for refs, _ in ways.values() for r in refs}
    kept_nodes = {nid: ll for nid, ll in nodes.items() if nid in referenced}
    missing = len(referenced) - len(kept_nodes)

    with open(args.out, "w", encoding="utf-8") as f:
        f.write('<?xml version="1.0" encoding="UTF-8"?>\n')
        f.write('<osm version="0.6" generator="fetch_road_extract.py" '
                'copyright="OpenStreetMap contributors" '
                'license="http://opendatacommons.org/licenses/odbl/1-0/">\n')
        for nid, (nlat, nlon) in kept_nodes.items():
            f.write(f'  <node id="{nid}" lat="{nlat}" lon="{nlon}"/>\n')
        for wid, (refs, tags) in ways.items():
            f.write(f'  <way id="{wid}">\n')
            for r in refs:
                f.write(f'    <nd ref="{r}"/>\n')
            for k, v in tags.items():
                v = (v.replace("&", "&amp;").replace('"', "&quot;")
                      .replace("<", "&lt;").replace(">", "&gt;"))
                f.write(f'    <tag k="{k}" v="{v}"/>\n')
            f.write("  </way>\n")
        f.write("</osm>\n")

    print(f"\nwrote {args.out}: {len(kept_nodes)} nodes, {len(ways)} road ways, "
          f"{tiles} tiles ({missing} refs fell outside the fetched area)")


if __name__ == "__main__":
    main()
