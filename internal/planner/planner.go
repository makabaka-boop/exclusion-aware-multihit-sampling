// Package planner implements the frozen-aware minimum hitting-point solver.
//
// Given a collection of closed integer risk intervals and a collection of
// closed integer frozen intervals, it finds the smallest set of batch numbers
// such that every risk interval contains at least RequiredHits (1..3, default
// 1) DISTINCT selected numbers and no selected number lies inside a frozen
// segment. One selected number may still hit many different intervals; it can
// only ever count once towards a single interval.
package planner

import (
	"sort"
)

// Interval is one risk interval as it arrives from the request.
type Interval struct {
	ID    string `json:"id"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	// RequiredHits is the number of distinct selected batches this interval
	// must contain. Valid inputs are 1..3; zero means "unset" and keeps the
	// original single-hit semantics.
	RequiredHits int `json:"requiredHits,omitempty"`
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

// Point groups a selected batch number with the risk intervals it lies in.
type Point struct {
	Batch   int64    `json:"batch"`
	Covered []string `json:"covered_ids"`
}

// IntervalHits lists the selected batches that actually hit one risk interval,
// in ascending order. Entries follow the processing order (see Result.Order).
type IntervalHits struct {
	IntervalID string  `json:"interval_id"`
	Batches    []int64 `json:"batches"`
}

// Failure identifies the first risk interval (in processing order) that
// cannot obtain enough distinct selectable batch numbers.
type Failure struct {
	IntervalID string `json:"interval_id"`
	Position   int    `json:"failed_position"`
	// Missing is the gap: how many additional selectable integer batches the
	// interval would need beyond those available inside it
	// (RequiredHits minus the number of non-frozen integers in the interval).
	Missing int `json:"failed_missing"`
}

// Result is the normalized outcome of a planning run.
type Result struct {
	// Status is "OK" when a plan exists, "NO_SAMPLE_POINT" otherwise.
	Status string
	// Points carries the complete plan, ascending by batch; it is nil (never
	// partial) on failure.
	Points []Point
	// Hits carries, for every interval in processing order, the selected
	// batches actually lying inside it. It is nil on failure.
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

// rightmostAvailable returns the largest integer p with lo <= p <= hi that is
// neither frozen nor already selected (present in the strictly ascending,
// distinct used slice), or (0, false) when no such integer exists.
func rightmostAvailable(seg []Segment, used []int64, lo, hi int64) (int64, bool) {
	for {
		p, ok := rightmostFree(seg, lo, hi)
		if !ok {
			return 0, false
		}
		j := sort.Search(len(used), func(i int) bool { return used[i] >= p })
		if j == len(used) || used[j] != p {
			return p, true
		}
		// p already serves as a sample; keep scanning the integers below it.
		hi = p - 1
	}
}

// insertBatch keeps the selected batches sorted and distinct.
func insertBatch(batches []int64, p int64) []int64 {
	j := sort.Search(len(batches), func(i int) bool { return batches[i] >= p })
	batches = append(batches, 0)
	copy(batches[j+1:], batches[j:])
	batches[j] = p
	return batches
}

// Plan computes the minimum sampling plan.
//
// The intervals are processed in order of increasing right endpoint (ties by
// id, then input order). For each interval the planner counts the DISTINCT
// already-selected batches it contains; one batch can never count twice within
// the same interval. When fewer than RequiredHits batches are present, it adds
// the missing batches from right to left, each being the largest unused
// non-frozen integer not exceeding the previous pick. Choosing as far right as
// possible is optimal by the standard exchange argument: any feasible solution
// must place some point no greater than the chosen one inside the current
// interval, and replacing it with the chosen point cannot lose coverage of an
// interval processed later (whose right endpoint is no smaller).
//
// If an interval cannot obtain enough distinct selectable batches, planning
// aborts immediately: NO_SAMPLE_POINT names the first failing interval in
// processing order and the gap, and no partial plan is published.
func Plan(intervals []Interval, frozen []FrozenRange) Result {
	seg := NormalizeFrozen(frozen)
	order := processingOrder(intervals)

	orderIDs := make([]string, len(order))
	for pos, i := range order {
		orderIDs[pos] = intervals[i].ID
	}

	batches := make([]int64, 0)

	for pos, i := range order {
		iv := intervals[i]
		need := iv.RequiredHits
		if need < 1 {
			need = 1 // unset keeps the original single-hit semantics
		}

		// Distinct selected batches already lying inside [Start, End].
		left := sort.Search(len(batches), func(j int) bool { return batches[j] >= iv.Start })
		right := sort.Search(len(batches), func(j int) bool { return batches[j] > iv.End })
		need -= right - left

		cursor := iv.End
		for need > 0 {
			p, ok := rightmostAvailable(seg, batches, iv.Start, cursor)
			if !ok {
				// Abort with no partial plan. `need` equals the gap: every
				// non-frozen integer in the interval is already selected.
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
			batches = insertBatch(batches, p)
			cursor = p - 1
			need--
		}
	}

	// Derive the per-interval hit lists from the final set and, as their
	// transpose, the intervals each batch covers. Lists are built in
	// processing order, which fixes the order inside Covered as well.
	points := make([]Point, len(batches))
	for j := range batches {
		points[j] = Point{Batch: batches[j], Covered: []string{}}
	}
	hits := make([]IntervalHits, len(order))
	for pos, i := range order {
		iv := intervals[i]
		left := sort.Search(len(batches), func(j int) bool { return batches[j] >= iv.Start })
		right := sort.Search(len(batches), func(j int) bool { return batches[j] > iv.End })
		b := append([]int64(nil), batches[left:right]...)
		hits[pos] = IntervalHits{IntervalID: iv.ID, Batches: b}
		for _, p := range b {
			j := sort.Search(len(batches), func(j int) bool { return batches[j] >= p })
			points[j].Covered = append(points[j].Covered, iv.ID)
		}
	}

	return Result{
		Status: "OK",
		Points: points,
		Hits:   hits,
		Order:  orderIDs,
	}
}
