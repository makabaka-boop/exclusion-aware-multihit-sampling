// Package planner implements the frozen-aware minimum hitting-point solver.
//
// Given a collection of closed integer risk intervals and a collection of
// closed integer frozen intervals, it finds the smallest set of batch numbers
// such that every risk interval contains at least one selected number and no
// selected number lies inside a frozen segment.
package planner

import (
	"sort"
)

// Interval is one risk interval as it arrives from the request.
type Interval struct {
	ID    string `json:"id"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
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

// Failure identifies the first risk interval (in processing order) that
// contains no selectable batch number.
type Failure struct {
	IntervalID string `json:"interval_id"`
	Position   int    `json:"failed_position"`
}

// Result is the normalized outcome of a planning run.
type Result struct {
	// Status is "OK" when a plan exists, "NO_SAMPLE_POINT" otherwise.
	Status string
	// Points carries the complete plan; it is nil (never partial) on failure.
	Points []Point
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
// id, then input order). An interval already containing the most recently
// selected point is covered "for free". Otherwise the largest available
// integer not exceeding its right endpoint is selected. Selecting as far right
// as possible is the standard exchange-argument optimum for minimum hitting
// points, and jumping over frozen segments preserves that optimality because
// any feasible solution must pick some point no greater than the one chosen,
// which the chosen point can replace without losing later coverage.
func Plan(intervals []Interval, frozen []FrozenRange) Result {
	seg := NormalizeFrozen(frozen)
	order := processingOrder(intervals)

	orderIDs := make([]string, len(order))
	for pos, i := range order {
		orderIDs[pos] = intervals[i].ID
	}

	points := make([]Point, 0)
	var lastBatch int64

	for pos, i := range order {
		iv := intervals[i]

		// Points are strictly increasing, so only the latest selected point
		// can possibly lie inside this interval.
		if n := len(points); n > 0 && lastBatch >= iv.Start {
			points[n-1].Covered = append(points[n-1].Covered, iv.ID)
			continue
		}

		p, ok := rightmostFree(seg, iv.Start, iv.End)
		if !ok {
			// Abort with no partial plan.
			return Result{
				Status: "NO_SAMPLE_POINT",
				Order:  orderIDs,
				Fail: Failure{
					IntervalID: iv.ID,
					Position:   pos + 1,
				},
			}
		}

		lastBatch = p
		points = append(points, Point{
			Batch:   p,
			Covered: []string{iv.ID},
		})
	}

	return Result{
		Status: "OK",
		Points: points,
		Order:  orderIDs,
	}
}
