package service

import (
	"strings"
	"testing"
	"time"

	"shingocore/store/cms"
)

// health builds a FeedHealth for the verdict tests. Everything defaults to the
// shape of a working feed, so each case changes exactly one thing.
func health(mutate func(*FeedHealth)) *FeedHealth {
	posted := time.Now().Add(-time.Minute)
	h := &FeedHealth{
		Enabled:              true,
		ConfiguredStorerooms: 2,
		Health: &cms.Health{
			Posted:         10,
			PostedLastHour: 4,
			LastPostedAt:   &posted,
		},
	}
	if mutate != nil {
		mutate(h)
	}
	return h
}

// TestVerdict_HealthyNeedsPOSITIVEEvidence is the rule the whole surface exists
// for. "Nothing is pending" is not health — it is what a feed nobody is handing
// work to looks like, and what a muted poster looks like, and what an untagged
// plant looks like. Healthy requires a successful post and a tagged boundary.
func TestVerdict_HealthyNeedsPOSITIVEEvidence(t *testing.T) {
	t.Parallel()
	h := health(nil)
	h.Verdict()
	if !h.Healthy {
		t.Fatalf("a feed with recent posts and tagged boundaries is not healthy: %s", h.Why)
	}

	// A perfectly quiet queue with NOTHING ever posted is not healthy.
	quiet := health(func(h *FeedHealth) {
		h.LastPostedAt = nil
		h.PostedLastHour = 0
		h.Posted = 0
	})
	quiet.Verdict()
	if quiet.Healthy {
		t.Error("a feed that has never posted anything reported healthy — an empty queue " +
			"is not evidence that the pipe works")
	}
	if !strings.Contains(quiet.Why, "nothing has been posted yet") {
		t.Errorf("why = %q, want it to say the feed is unproven", quiet.Why)
	}
}

// TestVerdict_NamesTheWorstProblemFirst. A page reporting the mildest of
// several problems sends someone to fix the wrong one, so the order is not
// cosmetic. Each case here adds its condition ON TOP of the ones below it and
// asserts the more serious one wins.
func TestVerdict_NamesTheWorstProblemFirst(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*FeedHealth)
		wantSub string
		why     string
	}{
		{
			name: "a lost movement outranks everything",
			mutate: func(h *FeedHealth) {
				h.BuildFailures = 2
				h.Muted, h.MutedReason = true, "bad key"
				h.Failed, h.Pending = 5, 100
				h.OldestPendingAgeSeconds = 99999
			},
			wantSub: "could not be turned into CMS transactions",
			why:     "it is the only failure that leaves no row anywhere — every other one is visible in a table",
		},
		{
			name: "a muted poster outranks a backlog",
			mutate: func(h *FeedHealth) {
				h.Muted, h.MutedReason = true, "credential fault on posting 3"
				h.Pending, h.OldestPendingAgeSeconds = 100, 99999
			},
			wantSub: "muted",
			why:     "the backlog is a SYMPTOM of the mute; reporting it sends someone to the wrong place",
		},
		{
			name: "no tagged boundaries outranks a stale queue",
			mutate: func(h *FeedHealth) {
				h.ConfiguredStorerooms = 0
				h.OldestPendingAgeSeconds = 99999
			},
			wantSub: "cms_storeroom",
			why:     "with nothing tagged there is nothing to post; the age is meaningless",
		},
		{
			name: "an unresolvable inflight posting outranks a failed one",
			mutate: func(h *FeedHealth) {
				h.UnresolvableInflight = 1
				h.Failed = 3
			},
			wantSub: "never acknowledged with a transaction id",
			why:     "a failed row can be retried; an unresolvable one needs a human at the middleware",
		},
		{
			name: "unqueued transactions outrank failed postings",
			mutate: func(h *FeedHealth) {
				h.Unposted = 7
				h.Failed = 1
			},
			wantSub: "never queued for posting",
			why:     "a failing subscriber is losing NEW work; a failed posting is one batch",
		},
		{
			name: "a stale pending queue is reported when nothing worse is wrong",
			mutate: func(h *FeedHealth) {
				h.Pending, h.OldestPendingAgeSeconds = 4, 3600
			},
			wantSub: "not draining",
			why:     "the age is what makes a backlog a finding; the count alone cannot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := health(tc.mutate)
			h.Verdict()
			if h.Healthy {
				t.Fatalf("reported healthy: %s", h.Why)
			}
			if !strings.Contains(h.Why, tc.wantSub) {
				t.Errorf("why = %q, want it to mention %q — %s", h.Why, tc.wantSub, tc.why)
			}
		})
	}
}

// TestVerdict_DisabledIsNotUnhealthy: a site with no cms: block is correctly
// configured. Rendering it as a broken feed would train people to ignore the
// card at the one plant where it means something.
func TestVerdict_DisabledIsNotUnhealthy(t *testing.T) {
	t.Parallel()
	h := &FeedHealth{Enabled: false, Health: &cms.Health{}}
	h.Verdict()
	if h.Healthy {
		t.Error("a disabled feed reported healthy — it has nothing to be healthy about")
	}
	// And it must not accumulate a scary sentence from the zero counts.
	if strings.Contains(h.Why, "not draining") || strings.Contains(h.Why, "cms_storeroom") {
		t.Errorf("why = %q, want no finding about a feed that is not configured", h.Why)
	}
}

// TestVerdict_SentenceSaysWhatTheEvidenceIS. When it says healthy, the reason
// has to be checkable — "why do you think it is fine" is as much a diagnostic
// question as the other one, and a bare "OK" cannot be argued with.
func TestVerdict_SentenceSaysWhatTheEvidenceIS(t *testing.T) {
	t.Parallel()
	h := health(nil)
	h.Verdict()
	for _, want := range []string{"last successful post", "in the last hour", "tagged boundary node"} {
		if !strings.Contains(h.Why, want) {
			t.Errorf("healthy sentence %q does not mention %q", h.Why, want)
		}
	}
}

func TestCountOf_Pluralises(t *testing.T) {
	t.Parallel()
	if got := countOf(1, "posting"); got != "1 posting" {
		t.Errorf("countOf(1) = %q", got)
	}
	if got := countOf(3, "posting"); got != "3 postings" {
		t.Errorf("countOf(3) = %q", got)
	}
	if got := countOf(0, "posting"); got != "0 postings" {
		t.Errorf("countOf(0) = %q", got)
	}
}

func TestForSeconds_CoarsensWithAge(t *testing.T) {
	t.Parallel()
	// A backlog measured in seconds and one measured in hours are different
	// findings and must not read the same.
	cases := map[int64]string{30: "30s", 119: "119s", 300: "5m", 7199: "119m", 7200: "2h", 86400: "24h"}
	for in, want := range cases {
		if got := forSeconds(in); got != want {
			t.Errorf("forSeconds(%d) = %q, want %q", in, got, want)
		}
	}
}
