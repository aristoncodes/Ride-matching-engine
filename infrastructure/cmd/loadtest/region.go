package main

import (
	"fmt"
	"math"
	"math/rand"
)

// region selects the geographic profile the generators draw from.
type region string

const (
	// regionCentre is the original profile: a small box around the city centre.
	// Its output must stay byte-for-byte identical so earlier runs stay comparable.
	regionCentre region = "centre"
	// regionBengaluru is the city-wide profile with demand-weighted pickups.
	regionBengaluru region = "bengaluru"
)

const (
	centreLat = 12.9716
	centreLng = 77.5946

	// Bengaluru urban bounding box.
	bngLatMin, bngLatMax = 12.70, 13.20
	bngLngMin, bngLngMax = 77.40, 77.80

	hotspotSigma     = 0.012 // degrees, roughly 1.3 km
	uniformBackgroud = 0.15  // weight of the uniform background in rider demand
)

type hotspot struct {
	name     string
	lat, lng float64
	weight   float64
}

// bengaluruHotspots are ASSUMED starting values, to be checked against real
// demand data. They are not measured. Hotspot weights are absolute shares of
// rider demand and, with the 0.15 uniform background, sum to 1.0.
var bengaluruHotspots = []hotspot{
	{"CBD / MG Road", 12.9716, 77.5946, 0.25},
	{"Whitefield", 12.9698, 77.7500, 0.15},
	{"Electronic City", 12.8399, 77.6770, 0.12},
	{"Koramangala", 12.9352, 77.6245, 0.12},
	{"Indiranagar", 12.9784, 77.6408, 0.08},
	{"Kempegowda Airport", 13.1986, 77.7066, 0.08},
	{"Hebbal", 13.0358, 77.5970, 0.05},
}

func parseRegion(s string) (region, error) {
	switch region(s) {
	case regionCentre, regionBengaluru:
		return region(s), nil
	}
	return "", fmt.Errorf("unknown region %q (want centre | bengaluru)", s)
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func uniformBengaluru(rng *rand.Rand) (float64, float64) {
	return bngLatMin + rng.Float64()*(bngLatMax-bngLatMin),
		bngLngMin + rng.Float64()*(bngLngMax-bngLngMin)
}

// uniformPoint draws a driver position. For centre it is the original
// span-degree box around the centre (two draws, lat then lng); for bengaluru it
// is uniform over the city box and span is ignored.
func (r region) uniformPoint(rng *rand.Rand, span float64) (lat, lng float64) {
	if r == regionBengaluru {
		return uniformBengaluru(rng)
	}
	return centreLat + (rng.Float64()-0.5)*span, centreLng + (rng.Float64()-0.5)*span
}

// riderPoint draws a pickup position. Centre is uniform (as before); bengaluru
// is a mixture of a uniform background and Gaussian hotspots, clamped to the box.
func (r region) riderPoint(rng *rand.Rand, span float64) (lat, lng float64) {
	if r != regionBengaluru {
		return r.uniformPoint(rng, span)
	}
	if rng.Float64() < uniformBackgroud {
		return uniformBengaluru(rng)
	}
	u := rng.Float64() * (1 - uniformBackgroud)
	h := bengaluruHotspots[len(bengaluruHotspots)-1]
	acc := 0.0
	for _, c := range bengaluruHotspots {
		acc += c.weight
		if u < acc {
			h = c
			break
		}
	}
	lat = clamp(h.lat+rng.NormFloat64()*hotspotSigma, bngLatMin, bngLatMax)
	lng = clamp(h.lng+rng.NormFloat64()*hotspotSigma, bngLngMin, bngLngMax)
	return lat, lng
}
