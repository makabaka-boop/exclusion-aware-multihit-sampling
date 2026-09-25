// Package planner implements the frozen-aware minimum hitting-point solver.
//
// Given a collection of closed integer risk intervals, each demanding
// RequiredHits distinct sample batches, and a collection of closed integer
// frozen segments, it finds the smallest set of batch numbers such that
// every risk interval contains at least RequiredHits selected numbers and
// no selected number lies inside a frozen segment. One selected batch may
// serve several different intervals, but it never counts twice for the
// same interval.
package planner

import (
	"slices"
	"sort"
)

// Interval is one risk interval as it arrives from the request.
type Interval struct {
	ID    string `json:"id"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	// RequiredHits is the number of distinct batch numbers that must be
	// sampled inside [Start, End]. Zero means "not set" and keeps the
	// legacy single-hit semantics. The API layer restricts values to 1..3.
	RequiredHits int `json:"required_hits,omitempty"`
}

// FrozenRange is one frozen segment as it arrives from the request.
type FrozenRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Segment is a normalized (disjoint, sorted) frozen segment.
type Segment struct {
	Start int64
	End   int64
}

// Point groups a selected batch number with the risk intervals it covers.
type Point struct {
	Batch   int64    `json:"batch"`
	Covered []string `json:"covered_ids"`
}

// IntervalHits lists the distinct sample batches that satisfy one risk
// interval, ascending. Hits are reported in the intervals' processing order.
type IntervalHits struct {
	ID      string  `json:"id"`
	Batches []int64 `json:"batches"`
}

// Failure identifies the first risk interval (in processing order) whose
// usable batch numbers are insufficient for its RequiredHits.
type Failure struct {
	IntervalID string `json:"interval_id"`
	Position   int    `json:"failed_position"`
	// Missing is the gap: how many more usable batch numbers the interval
	// would need to satisfy its RequiredHits.
	Missing int `json:"missing"`
}

// Result is the normalized outcome of a planning run.
type Result struct {
	// Status is "OK" when a plan exists, "NO_SAMPLE_POINT" otherwise.
	Status string
	// Points carries the complete plan; it is nil (never partial) on failure.
	Points []Point
	// Hits lists, in processing order, the batches hitting each interval;
	// it is nil (never partial) on failure.
	Hits []IntervalHits
	// Order is the processing order, i.e. interval IDs sorted by
	// (end asc, id asc, original index asc).
	Order []string
	// Fail is set only when Status == "NO_SAMPLE_POINT".
	Fail Failure
}

// NormalizeFrozen merges the frozen ranges into a sorted list of pairwise
// disjoint, non-touching closed integer segments. Segments that overlap or are
// adjacent (e.g. [1,2] and [3,4], which together forbid 1..4 with no integer
// gap) are merged.
func NormalizeFrozen(ranges []FrozenRange) []Segment {
	if len(ranges) == 0 {
		return nil
	}

	sorted := make([]Segment, 0, len(ranges))
	for _, r := range ranges {
		sorted = append(sorted, Segment{Start: r.Start, End: r.End})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End < sorted[j].End
	})

	merged := make([]Segment, 0, len(sorted))
	for _, s := range sorted {
		last := len(merged) - 1
		// Closed integer segments [a,b], [c,d] with c <= b+1 leave no
		// selectable integer between them, so they join into one segment.
		if last >= 0 && s.Start <= merged[last].End+1 {
			if s.End > merged[last].End {
				merged[last].End = s.End
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// processingOrder returns the intervals stably sorted by (end asc, id asc).
// The stable sort keeps the input order between equal keys, which is required
// so the processing order is fully deterministic and reproducible.
func processingOrder(intervals []Interval) []int {
	idx := make([]int, len(intervals))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := intervals[idx[a]], intervals[idx[b]]
		if x.End != y.End {
			return x.End < y.End
		}
		return x.ID < y.ID
	})
	return idx
}

// rightmostFree returns the largest integer p with lo <= p <= hi that is not
// covered by any frozen segment, or (0, false) when [lo,hi] is fully frozen.
//
// seg is the output of NormalizeFrozen: segments are disjoint, sorted by start
// and separated by at least one free integer.
func rightmostFree(seg []Segment, lo, hi int64) (int64, bool) {
	if hi < lo {
		return 0, false
	}
	// Find the last segment starting at or below hi.
	j := sort.Search(len(seg), func(i int) bool { return seg[i].Start > hi }) - 1
	if j < 0 || seg[j].End < hi {
		// No segment reaches hi, so hi itself is selectable.
		return hi, true
	}
	// hi lies inside seg[j]. The integer just before the segment is free:
	// normalization guarantees seg[j-1].End <= seg[j].Start-2, so nothing
	// covers seg[j].Start-1. It only remains to check the lower bound.
	p := seg[j].Start - 1
	if p < lo {
		return 0, false
	}
	return p, true
}

// Plan computes the minimum sampling plan.
//
// The intervals are processed in order of increasing right endpoint (ties by
// id, then input order). For each interval, the distinct batches already
// selected inside it are counted; when fewer than RequiredHits are present,
// new batches are picked from right to left — the largest usable integer not
// exceeding the cursor — skipping frozen segments and batches already
// selected (a batch may not serve the same interval twice), until the demand
// is met. Selecting as far right as possible is the standard
// exchange-argument optimum for minimum hitting points, and jumping over
// frozen segments preserves that optimality because any feasible solution
// must pick some point no greater than the one chosen, which the chosen
// point can replace without losing later coverage.
//
// When an interval does not contain enough usable integers, Plan aborts with
// the first such interval in processing order and the exact gap, publishing
// no partial plan.
func Plan(intervals []Interval, frozen []FrozenRange) Result {
	seg := NormalizeFrozen(frozen)
	order := processingOrder(intervals)

	orderIDs := make([]string, len(order))
	for pos, i := range order {
		orderIDs[pos] = intervals[i].ID
	}

	var batches []int64             // selected batch numbers, strictly increasing
	covered := map[int64][]string{} // batch -> intervals it serves, in processing order
	hits := make([]IntervalHits, 0, len(intervals))

	for pos, i := range order {
		iv := intervals[i]
		k := iv.RequiredHits
		if k <= 0 {
			k = 1 // unset keeps the legacy single-hit semantics
		}

		// Processing order has non-decreasing right endpoints and every
		// selected batch is <= its interval's right endpoint, so the batches
		// inside this interval are exactly the suffix at or above iv.Start.
		j := sort.Search(len(batches), func(b int) bool { return batches[b] >= iv.Start })
		inside := len(batches) - j

		// The interval is served by its largest existing batches (up to k).
		keep := k
		if inside < keep {
			keep = inside
		}
		assigned := make([]int64, 0, k)
		for _, b := range batches[len(batches)-keep:] {
			covered[b] = append(covered[b], iv.ID)
			assigned = append(assigned, b)
		}

		// Pick the missing samples from right to left.
		cursor := iv.End
		for need := k - keep; need > 0; {
			p, ok := rightmostFree(seg, iv.Start, cursor)
			if !ok {
				// Every usable integer inside the interval is already
				// selected, so need is exactly the gap. Abort with no
				// partial plan.
				return Result{
					Status: "NO_SAMPLE_POINT",
					Order:  orderIDs,
					Fail: Failure{
						IntervalID: iv.ID,
						Position:   pos + 1,
						Missing:    need,
					},
				}
			}
			at := sort.Search(len(batches), func(b int) bool { return batches[b] >= p })
			if at < len(batches) && batches[at] == p {
				// Already selected: it was counted in `inside`, so it may
				// not serve this interval twice. Continue left of it.
				cursor = p - 1
				continue
			}
			// Insert p, keeping batches sorted.
			batches = append(batches, 0)
			copy(batches[at+1:], batches[at:])
			batches[at] = p
			covered[p] = []string{iv.ID}
			assigned = append(assigned, p)
			cursor = p - 1
			need--
		}

		slices.Sort(assigned)
		hits = append(hits, IntervalHits{ID: iv.ID, Batches: assigned})
	}

	points := make([]Point, 0, len(batches))
	for _, b := range batches {
		points = append(points, Point{Batch: b, Covered: covered[b]})
	}

	return Result{
		Status: "OK",
		Points: points,
		Hits:   hits,
		Order:  orderIDs,
	}
}
