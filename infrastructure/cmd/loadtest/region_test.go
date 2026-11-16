package main

import (
	"math"
	"testing"
)

func TestCentreUnchanged(t *testing.T) {
	r := buildBatch(5, 4, 8)
	wantR := [][2]float64{
		{13.015088244655663, 77.53941454683817},
		{12.931979579764302, 77.60056439627171},
		{13.015247364305612, 77.64036495565072},
		{12.885255962105443, 77.53419988203139},
		{13.048647828505548, 77.59352208282316},
	}
	wantD := [][2]float64{
		{13.018408043560568, 77.6113185826257},
		{12.988819436343613, 77.58042900507327},
		{13.006402625666958, 77.6539321362679},
		{12.998805555648586, 77.67158087896611},
	}
	for i, w := range wantR {
		p := r.Riders[i].Pickup
		if p.Lat != w[0] || p.Lng != w[1] {
			t.Errorf("rider %d = %v,%v want %v", i, p.Lat, p.Lng, w)
		}
	}
	for i, w := range wantD {
		p := r.CandidateDrivers[i].Location
		if p.Lat != w[0] || p.Lng != w[1] {
			t.Errorf("driver %d = %v,%v want %v", i, p.Lat, p.Lng, w)
		}
	}
}

func TestBengaluruBoundsAndDeterminism(t *testing.T) {
	a := buildBatchRegion(10000, 5000, 8, regionBengaluru)
	b := buildBatchRegion(10000, 5000, 8, regionBengaluru)
	in := func(lat, lng float64) bool {
		return lat >= bngLatMin && lat <= bngLatMax && lng >= bngLngMin && lng <= bngLngMax
	}
	for i, x := range a.Riders {
		if !in(x.Pickup.Lat, x.Pickup.Lng) {
			t.Fatalf("rider %d outside box", i)
		}
		if x.Pickup.Lat != b.Riders[i].Pickup.Lat || x.Pickup.Lng != b.Riders[i].Pickup.Lng {
			t.Fatalf("rider %d not deterministic", i)
		}
	}
	for i, x := range a.CandidateDrivers {
		if !in(x.Location.Lat, x.Location.Lng) {
			t.Fatalf("driver %d outside box", i)
		}
	}
}

func nearCBD(lat, lng float64) bool {
	return math.Hypot(lat-centreLat, lng-centreLng) <= 0.05
}

func TestBengaluruDemandWeighting(t *testing.T) {
	req := buildBatchRegion(10000, 10000, 8, regionBengaluru)
	var rn, dn int
	for _, x := range req.Riders {
		if nearCBD(x.Pickup.Lat, x.Pickup.Lng) {
			rn++
		}
	}
	for _, x := range req.CandidateDrivers {
		if nearCBD(x.Location.Lat, x.Location.Lng) {
			dn++
		}
	}
	rf, df := float64(rn)/10000, float64(dn)/10000
	if rf <= 0.25 {
		t.Errorf("riders near CBD = %.3f, want > 0.25", rf)
	}
	if df >= 0.25 {
		t.Errorf("drivers near CBD = %.3f, want < 0.25", df)
	}
}

func TestHotspotWeightsSumToOne(t *testing.T) {
	s := uniformBackgroud
	for _, h := range bengaluruHotspots {
		s += h.weight
	}
	if math.Abs(s-1) > 1e-9 {
		t.Errorf("weights sum %v", s)
	}
	if _, err := parseRegion("nope"); err == nil {
		t.Error("expected error")
	}
}
