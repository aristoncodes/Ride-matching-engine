# Report: the committed Bengaluru road extract

**Subject:** `matching_engine/data/bengaluru_roads.osm`, the one data file the repo commits on purpose, and the routing claims quoted against it.
**Scope of this report:** what the file is, where it came from, what the claims rest on, what I reproduced, and what the file does not cover.
**Written for:** the repo owner, to check claims before using them in a README, a resume, or an interview.

## 1. What the file is

| | |
|---|---|
| Format | OpenStreetMap XML (`osm version 0.6`) |
| Nodes | 27,890 |
| Ways | 7,811 |
| Generator (from the file header) | `fetch_road_extract.py` |
| Generator script | `matching_engine/tools/fetch_road_extract.py` (present in the repo) |
| Licence (from the file header) | ODbL, OpenStreetMap contributors |
| Git history | Added 4 Aug 2026 in commit `838497d` (Week 4 routing) |
| Size | 3.4 MB |

The file records its generator but not the date it was downloaded, so a reader cannot tell from the file alone when the map data was current.

## 2. Coverage: central Bengaluru only

Computed from the node coordinates:

- Latitude **12.9409 to 13.0015** N
- Longitude **77.5674 to 77.6394** E

That is roughly 7 km by 8 km, in the centre of the city. It is **not** the whole of Bengaluru, which spans about 40 to 50 km. Any routing number quoted from this file describes the centre only.

This matters for the city-wide load profile we just built. The load generator draws riders and drivers across the whole city, but the routing model only knows the centre. Routing-based results should not be extended to the outer city without a larger extract.

## 3. Claims that depend on this file

From `docs/Benchmarks.md` §3–4, `docs/Technical_Design_Document.md`, `README.md` and `docs/api/matching.proto`:

| Claim | Stated value | Source |
|---|---|---|
| Mean detour factor (road distance over straight line) | 1.51× | Benchmarks §4, README |
| Assignments that change when matched on road time | 63% (38 of 60) | Benchmarks §4 |
| Rider waiting saved by road-time matching | 9.5% of total wait | Benchmarks §4 |
| Worst single wait | Rose from 8.7 to 9.3 min | Benchmarks §4 |
| Hot-origin cache agrees with fresh Dijkstra | 200 of 200 | Benchmarks §3 |
| A* settles fewer nodes than Dijkstra | 1.82× fewer | Benchmarks §3 (demo output) |

**Checked, with one caveat.** I re-ran the experiment (`matching_engine/build/road_match_demo data/bengaluru_roads.osm 60 60`):

- Pairings changed: **38 of 60 (63.3%)**. Matches the doc.
- Total waiting saved: **14.9 min (9.5%)**. Matches the doc.
- Worst single wait: **8.7 min to 9.3 min**. Matches the doc.
- Two runs gave identical output, because the sampling uses a fixed seed (`20260730`).

The caveat: the seed fixes one sample, not the distribution. A 120-rider run on the same extract gave **70.8% of pairings changed** and **11.9% of waiting saved**, with the worst wait going from 6.6 to 8.6 minutes. So the 63% and 9.5% figures are one small-sample point estimate. They reproduce, but they are not a stable effect size. Quote them with the sample size, or run several seeds before quoting a range.

## 4. What I reproduced

I ran the committed demo binary against the committed extract:

```
./build/route_demo data/bengaluru_roads.osm 200
```

Results:

- **Mean detour factor: 1.51×** over 200 random pairs. This matches the documented value.
- **A* vs Dijkstra: cost identical on every pair** (worst difference 0.00e+00 s), and the demo prints PASS.
- **Hot-origin cache: 200 of 200 probes agree** with a fresh search, using 0.85 MB.
- **Unreachable pairs: 0.**

Reproduced, but with caveats (section 5):

- **A* was not faster in wall-clock time.** It settles 1.82× fewer nodes, but the measured speedup was **1.02×**. The demo says so in its own output. Any README line that claims a speedup should use the 1.02× figure.
- **The 1.51× depends on which 200 pairs are sampled.** The demo draws pairs with a random generator. I did not confirm that the seed is fixed, so a re-run may give a slightly different mean.

## 5. Caveats

1. **Coverage.** Central Bengaluru only (section 2).
2. **Sample size.** The detour factor is a mean over 200 random pairs of nodes in the largest strongly connected component, not over all pairs or all trips.
3. **Straight-line baseline.** The factor compares road length with straight-line distance, which is the same simplification the matcher makes before the road-time change. It is a measure of how far straight-line distance understates trips, not a measured travel time.
4. **Freshness.** No download date is stored in the file, so the map data's age is unknown.
5. **Licence.** The file is ODbL data committed to a public repo. The header carries the OpenStreetMap attribution. Anyone redistributing it or quoting it should keep that attribution.

## 6. What to do before using these numbers externally

- Say "central Bengaluru" wherever the 1.51× appears, including the README and resume.
- Quote the A* speedup as 1.02×, not the node-count reduction.
- Store the download date in the file header, or in a sidecar note, so the data's age is recorded.
- If city-wide routing is claimed, fetch a larger extract with the existing script, which already handles bounding-box splits, and re-run `route_demo` on it.

## 7. Reproduction command

```
cd matching_engine
./build/route_demo data/bengaluru_roads.osm 200
```

The binary was already built in `matching_engine/build/`. I did not rebuild it.
