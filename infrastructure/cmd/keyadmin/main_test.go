package main

import (
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/auth"
)

// keyadmin is what an operator uses to reason about live credentials, so its
// DISPLAY is part of the security surface: a key shown as dead while it still
// authenticates is how a rotation gets abandoned half-finished, and a key
// shown as alive after revocation is how a compromise gets missed.
//
// The credential logic itself is covered in internal/auth. What is tested here
// is strictly the operator's-eye view of it.

var now = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func TestStatusAgreesWithEnforcement(t *testing.T) {
	cases := []struct {
		name       string
		key        auth.APIKey
		want       string
		wantActive bool
	}{
		{
			name:       "fresh key with no expiry",
			key:        auth.APIKey{},
			want:       "active",
			wantActive: true,
		},
		{
			name:       "expiry still in the future",
			key:        auth.APIKey{ExpiresAt: now.Add(30 * 24 * time.Hour)},
			want:       "active",
			wantActive: true,
		},
		{
			// The regression this file exists for. Rotate sets RevokedAt to
			// now+overlap, and the key goes on working until then — that IS
			// the zero-downtime rotation. Reporting "revoked" here told the
			// operator their migration was over while every client was still
			// authenticating on the old key.
			name:       "rotation overlap has not elapsed",
			key:        auth.APIKey{RevokedAt: now.Add(24 * time.Hour)},
			want:       "rotating",
			wantActive: true,
		},
		{
			name:       "rotation overlap has elapsed",
			key:        auth.APIKey{RevokedAt: now.Add(-time.Minute)},
			want:       "revoked",
			wantActive: false,
		},
		{
			name:       "revoked immediately",
			key:        auth.APIKey{RevokedAt: now},
			want:       "revoked",
			wantActive: false,
		},
		{
			name:       "past its expiry",
			key:        auth.APIKey{ExpiresAt: now.Add(-time.Hour)},
			want:       "expired",
			wantActive: false,
		},
		{
			// Revoked AND long expired. "revoked" is the more useful fact:
			// somebody took an action, as opposed to a key quietly lapsing.
			name: "revoked and also expired",
			key: auth.APIKey{
				RevokedAt: now.Add(-2 * time.Hour),
				ExpiresAt: now.Add(-time.Hour),
			},
			want:       "revoked",
			wantActive: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := status(&tc.key, now); got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}

			// The property that matters more than any single label: what the
			// operator is SHOWN must never contradict what the middleware
			// ENFORCES. Two independent implementations of "is this key
			// usable" is how they drift apart in the first place.
			active := tc.key.Active(now)
			if active != tc.wantActive {
				t.Fatalf("Active = %v, want %v (test case is wrong)", active, tc.wantActive)
			}
			shownAsUsable := status(&tc.key, now) == "active" || status(&tc.key, now) == "rotating"
			if shownAsUsable != active {
				t.Errorf("display says usable=%v but Active() says %v — an "+
					"operator would be misled about a live credential",
					shownAsUsable, active)
			}
		})
	}
}

func TestExpiryShowsTheNEARESTDeadline(t *testing.T) {
	cases := []struct {
		name string
		key  auth.APIKey
		want string
	}{
		{
			name: "no deadline at all",
			key:  auth.APIKey{},
			want: "never",
		},
		{
			name: "expiry only",
			key:  auth.APIKey{ExpiresAt: time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)},
			want: "2027-01-15",
		},
		{
			// A rotation overlap inside a key that is otherwise valid for a
			// year. Showing 2027 here would read as "plenty of time" while the
			// key actually stops working tomorrow.
			name: "pending revocation lands before the expiry",
			key: auth.APIKey{
				ExpiresAt: time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC),
				RevokedAt: now.Add(20 * time.Hour),
			},
			want: "2026-09-07 08:00",
		},
		{
			name: "revocation on a key with no expiry",
			key:  auth.APIKey{RevokedAt: now.Add(20 * time.Hour)},
			want: "2026-09-07 08:00",
		},
		{
			// Beyond a day, a date is enough and is easier to scan.
			name: "long overlap shows a plain date",
			key:  auth.APIKey{RevokedAt: now.Add(4 * 24 * time.Hour)},
			want: "2026-09-10",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expiry(&tc.key, now); got != tc.want {
				t.Errorf("expiry = %q, want %q", got, tc.want)
			}
		})
	}
}
