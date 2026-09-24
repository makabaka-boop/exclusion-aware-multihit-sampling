package planner

import (
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// bruteFreeSet returns the set of selectable integers within [lo,hi].
func bruteFreeSet(seg []Segment, lo, hi int64) map[int64]bool {
	frozen := map[int64]bool{}
	for _, s := range seg {
		for x := s.Start; x <= s.End; x++ {
			frozen[x] = true
		}
	}
	free := map[int64]bool{}
	for x := lo; x <= hi; x++ {
		if !frozen[x] {
			free[x] = true
		}
	}
	return free
}

// bruteMin enumerates every subset of the free integers on the union of
// interval endpoints' bounding box and returns the minimum subset size that
// hits every risk interval. ok=false when some interval is fully frozen.
func bruteMin(intervals []Interval, frozen []FrozenRange) (int, bool) {
	seg := NormalizeFrozen(frozen)

	var lo, hi int64 = intervals[0].Start, intervals[0].End
	for _, iv := range intervals[1:] {
		if iv.Start < lo {
			lo = iv.Start
		}
		if iv.End > hi {
			hi = iv.End
		}
	}

	free := bruteFreeSet(seg, lo, hi)
	for _, iv := range intervals {
		has := false
		for x := iv.Start; x <= iv.End; x++ {
			if free[x] {
				has = true
				break
			}
		}
		if !has {
			return 0, false
		}
	}

	points := make([]int64, 0, len(free))
	for p := range free {
		points = append(points, p)
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })

	// Enumerate subsets by mask; the problem instances in tests are small
	// (span <= ~12 free points), so this is tractable.
	n := len(points)
	best := n + 1
	for mask := 0; mask < 1<<n; mask++ {
		cnt := 0
		chosen := make([]bool, n)
		for b := 0; b < n; b++ {
			if mask&(1<<b) != 0 {
				chosen[b] = true
				cnt++
			}
		}
		if cnt >= best {
			continue
		}
		hitsAll := true
		for _, iv := range intervals {
			hit := false
			for b, p := range points {
				if chosen[b] && p >= iv.Start && p <= iv.End {
					hit = true
					break
				}
			}
			if !hit {
				hitsAll = false
				break
			}
		}
		if hitsAll && cnt < best {
			best = cnt
		}
	}
	return best, true
}

// verifyResult independently re-checks every invariant of a Plan result:
// feasibility, coverage, minimality against brute force, increasing batches,
// coverage partition, stable processing order and determinism.
func verifyResult(t *testing.T, intervals []Interval, frozen []FrozenRange) {
	t.Helper()

	res := Plan(intervals, frozen)
	res2 := Plan(intervals, frozen)
	if !reflect.DeepEqual(res, res2) {
		t.Fatalf("non-deterministic result:\n%+v\n%+v", res, res2)
	}

	wantOrder := make([]int, len(intervals))
	for i := range wantOrder {
		wantOrder[i] = i
	}
	sort.SliceStable(wantOrder, func(a, b int) bool {
		x, y := intervals[wantOrder[a]], intervals[wantOrder[b]]
		if x.End != y.End {
			return x.End < y.End
		}
		return x.ID < y.ID
	})
	wantIDs := make([]string, len(wantOrder))
	byID := map[string]Interval{}
	for i, iv := range intervals {
		wantIDs[i] = intervals[wantOrder[i]].ID
		byID[iv.ID] = iv
	}
	if !reflect.DeepEqual(res.Order, wantIDs) {
		t.Fatalf("processing order = %v, want %v", res.Order, wantIDs)
	}

	min, feasible := bruteMin(intervals, frozen)

	if !feasible {
		if res.Status != "NO_SAMPLE_POINT" {
			t.Fatalf("want NO_SAMPLE_POINT, got %+v", res)
		}
		if len(res.Points) != 0 {
			t.Fatalf("failure must not leave a partial plan, got %+v", res.Points)
		}
		// Failed interval must be the earliest (by processing order) fully frozen one.
		seg := NormalizeFrozen(frozen)
		earliest := ""
		var pos int
		for p, id := range res.Order {
			iv := byID[id]
			free := bruteFreeSet(seg, iv.Start, iv.End)
			if len(free) == 0 {
				earliest, pos = id, p+1
				break
			}
		}
		if res.Fail.IntervalID != earliest {
			t.Fatalf("failed interval = %q, want earliest %q", res.Fail.IntervalID, earliest)
		}
		if res.Fail.Position != pos {
			t.Fatalf("failed position = %d, want %d", res.Fail.Position, pos)
		}
		return
	}

	if res.Status != "OK" {
		t.Fatalf("want OK, got %+v", res)
	}
	if len(res.Points) != min {
		t.Fatalf("greedy picked %d points, brute-force minimum is %d\nintervals=%+v\nfrozen=%+v\nplan=%+v",
			len(res.Points), min, intervals, frozen, res.Points)
	}

	// Batches strictly increasing.
	for i := 1; i < len(res.Points); i++ {
		if res.Points[i-1].Batch >= res.Points[i].Batch {
			t.Fatalf("batches not strictly increasing: %+v", res.Points)
		}
	}

	// Every chosen batch is outside every frozen segment (normalized).
	seg := NormalizeFrozen(frozen)
	covered := map[string]bool{}
	assignedCount := 0
	for _, pt := range res.Points {
		for _, s := range seg {
			if pt.Batch >= s.Start && pt.Batch <= s.End {
				t.Fatalf("chosen batch %d lies in frozen segment %v", pt.Batch, s)
			}
		}
		for _, id := range pt.Covered {
			if covered[id] {
				t.Fatalf("interval %q covered by multiple points", id)
			}
			covered[id] = true
			assignedCount++
			iv := byID[id]
			if pt.Batch < iv.Start || pt.Batch > iv.End {
				t.Fatalf("batch %d does not cover interval %q = %v", pt.Batch, id, iv)
			}
		}
	}
	if assignedCount != len(intervals) {
		t.Fatalf("coverage partition has %d assignments, want %d", assignedCount, len(intervals))
	}
}

func TestNoFrozenClassic(t *testing.T) {
	// Classic greedy example.
	ivs := []Interval{
		{ID: "a", Start: 1, End: 3},
		{ID: "b", Start: 2, End: 6},
		{ID: "c", Start: 5, End: 8},
		{ID: "d", Start: 9, End: 10},
	}
	verifyResult(t, ivs, nil)

	res := Plan(ivs, nil)
	wantBatches := []int64{3, 8, 10}
	for i, w := range wantBatches {
		if res.Points[i].Batch != w {
			t.Fatalf("point %d = %d, want %d", i, res.Points[i].Batch, w)
		}
	}
}

func TestTouchingFrozenMerged(t *testing.T) {
	// [0,1] and [2,3] merely touch (b+1 adjacency) and must merge to [0,3].
	frozen := []FrozenRange{{Start: 0, End: 1}, {Start: 2, End: 3}}
	seg := NormalizeFrozen(frozen)
	if !reflect.DeepEqual(seg, []Segment{{Start: 0, End: 3}}) {
		t.Fatalf("touching segments not merged: %+v", seg)
	}

	// Overlapping duplicates and containment also collapse.
	frozen = []FrozenRange{{Start: 5, End: 9}, {Start: 6, End: 7}, {Start: 10, End: 12}, {Start: 9, End: 10}}
	seg = NormalizeFrozen(frozen)
	if !reflect.DeepEqual(seg, []Segment{{Start: 5, End: 12}}) {
		t.Fatalf("overlapping segments not merged: %+v", seg)
	}

	// [1,2] and [4,5] leave integer 3 free: they must NOT merge.
	frozen = []FrozenRange{{Start: 1, End: 2}, {Start: 4, End: 5}}
	seg = NormalizeFrozen(frozen)
	if !reflect.DeepEqual(seg, []Segment{{Start: 1, End: 2}, {Start: 4, End: 5}}) {
		t.Fatalf("gap segments wrongly merged: %+v", seg)
	}
}

func TestSinglePointIntervals(t *testing.T) {
	cases := []struct {
		name   string
		ivs    []Interval
		frozen []FrozenRange
		failID string
	}{
		{
			name:   "free single points all share one sample",
			ivs:    []Interval{{ID: "s1", Start: 7, End: 7}, {ID: "s2", Start: 7, End: 7}},
			failID: "",
		},
		{
			name:   "single point frozen => failure",
			ivs:    []Interval{{ID: "a", Start: 1, End: 2}, {ID: "b", Start: 5, End: 5}},
			frozen: []FrozenRange{{Start: 5, End: 5}},
			failID: "b",
		},
		{
			name:   "whole endpoint range frozen by touching pair",
			ivs:    []Interval{{ID: "a", Start: 4, End: 5}},
			frozen: []FrozenRange{{Start: 4, End: 4}, {Start: 5, End: 5}},
			failID: "a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verifyResult(t, tc.ivs, tc.frozen)
			if tc.failID != "" {
				res := Plan(tc.ivs, tc.frozen)
				if res.Fail.IntervalID != tc.failID {
					t.Fatalf("fail id = %q, want %q", res.Fail.IntervalID, tc.failID)
				}
			}
		})
	}
}

func TestSameRightEndpointStableOrder(t *testing.T) {
	// Identical ends are ordered by id; stable input order is the last tiebreak.
	ivs := []Interval{
		{ID: "zeta", Start: 0, End: 5},
		{ID: "alpha", Start: 0, End: 5},
		{ID: "mid", Start: 0, End: 5},
	}
	res := Plan(ivs, nil)
	wantOrder := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(res.Order, wantOrder) {
		t.Fatalf("order = %v, want %v", res.Order, wantOrder)
	}
	if len(res.Points) != 1 || res.Points[0].Batch != 5 {
		t.Fatalf("want single sample at 5, got %+v", res.Points)
	}
	if !reflect.DeepEqual(res.Points[0].Covered, wantOrder) {
		t.Fatalf("covered = %v, want %v", res.Points[0].Covered, wantOrder)
	}

	// Same end, same id ordering even when only the first gets the new sample.
	ivs2 := []Interval{
		{ID: "b", Start: 5, End: 5},
		{ID: "a", Start: 0, End: 5},
	}
	res2 := Plan(ivs2, nil)
	if !reflect.DeepEqual(res2.Order, []string{"a", "b"}) {
		t.Fatalf("order = %v", res2.Order)
	}
	if len(res2.Points) != 1 || !reflect.DeepEqual(res2.Points[0].Covered, []string{"a", "b"}) {
		t.Fatalf("plan = %+v", res2.Points)
	}
	verifyResult(t, ivs2, nil)
}

func TestFrozenPushesSampleLeft(t *testing.T) {
	// End is frozen, so the sample must move left, possibly covering fewer
	// later intervals and forcing extra points.
	ivs := []Interval{
		{ID: "a", Start: 1, End: 5},
		{ID: "b", Start: 6, End: 10},
		{ID: "c", Start: 5, End: 10},
	}
	frozen := []FrozenRange{{Start: 5, End: 5}, {Start: 10, End: 10}}
	verifyResult(t, ivs, frozen)

	res := Plan(ivs, frozen)
	// a -> 4 (5 frozen); b processed before c at end 10 -> 9 (10 frozen),
	// and c [5,10] is already covered by 9.
	if !reflect.DeepEqual(res.Order, []string{"a", "b", "c"}) {
		t.Fatalf("order = %v", res.Order)
	}
	if len(res.Points) != 2 || res.Points[0].Batch != 4 || res.Points[1].Batch != 9 {
		t.Fatalf("plan = %+v", res.Points)
	}
}

func TestFrozenBetweenIntervals(t *testing.T) {
	ivs := []Interval{
		{ID: "a", Start: 0, End: 2},
		{ID: "b", Start: 4, End: 6},
	}
	frozen := []FrozenRange{{Start: 2, End: 4}}
	verifyResult(t, ivs, frozen)
	res := Plan(ivs, frozen)
	if len(res.Points) != 2 || res.Points[0].Batch != 1 || res.Points[1].Batch != 6 {
		t.Fatalf("plan = %+v", res.Points)
	}
}

// itoaTest keeps the exhaustive loop free of strconv noise at call sites.
func itoaTest(n int) string { return strconv.Itoa(n) }

// TestExhaustiveSmall enumerates all combinations of risk intervals over a
// small coordinate universe and every frozen mask, cross-checking the greedy
// result against brute-force enumeration of all point subsets.
func TestExhaustiveSmall(t *testing.T) {
	const n = 5 // coordinates 0..4

	// Every non-empty closed interval over 0..4 (15 of them).
	type raw struct{ l, r int64 }
	var all []raw
	for l := int64(0); l < n; l++ {
		for r := l; r < n; r++ {
			all = append(all, raw{l, r})
		}
	}

	// Every possible frozen subset (as merged segments): iterate masks over
	// coordinates and convert runs to frozen ranges.
	cases := 0
	for fm := 0; fm < 1<<n; fm++ {
		var frozen []FrozenRange
		for x := 0; x < n; {
			if fm&(1<<x) != 0 {
				y := x
				for y+1 < n && fm&(1<<(y+1)) != 0 {
					y++
				}
				frozen = append(frozen, FrozenRange{Start: int64(x), End: int64(y)})
				x = y + 1
			} else {
				x++
			}
		}

		// Enumerate every singleton/pair/triple of intervals for every frozen
		// mask: 15 + C(16,2)=120 pairs + C(17,3)=680 triples per mask.
		// Each brute force scans at most 2^5 subsets, so this is cheap.
		emit := func(picks ...raw) {
			ivs := make([]Interval, len(picks))
			for k, p := range picks {
				ivs[k] = Interval{ID: "i" + itoaTest(k), Start: p.l, End: p.r}
			}
			verifyResult(t, ivs, frozen)
			cases++
		}
		for i := 0; i < len(all); i++ {
			emit(all[i])
			for j := i; j < len(all); j++ {
				emit(all[i], all[j])
				for k := j; k < len(all); k++ {
					emit(all[i], all[j], all[k])
				}
			}
		}
	}
	t.Logf("exhaustively verified %d small cases", cases)
}

// TestRandomizedLargeCoordinates fuzzes realistic instances including large
// coordinates (where brute force enumerates only the free points that actually
// matter), overlaps, touching segments and fully-frozen failures.
func TestRandomizedLargeCoordinates(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for iter := 0; iter < 400; iter++ {
		span := int64(1 + rng.Intn(11)) // coordinates 0..10
		ni := 1 + rng.Intn(5)
		ivs := make([]Interval, ni)
		usedIDs := map[string]bool{}
		for k := range ivs {
			l := rng.Int63n(span)
			r := l + rng.Int63n(span-l)
			id := ""
			for {
				id = string(rune('a'+rng.Intn(4))) + string(rune('a'+rng.Intn(4)))
				if !usedIDs[id] {
					usedIDs[id] = true
					break
				}
			}
			ivs[k] = Interval{ID: id, Start: l, End: r}
		}

		nf := rng.Intn(4)
		var frozen []FrozenRange
		for k := 0; k < nf; k++ {
			l := rng.Int63n(span)
			r := l + rng.Int63n(span-l)
			frozen = append(frozen, FrozenRange{Start: l, End: r})
		}
		verifyResult(t, ivs, frozen)
	}
}

func TestLargeCoordinateValues(t *testing.T) {
	// Exercises 10^9 boundaries without enumeration.
	ivs := []Interval{
		{ID: "a", Start: 1_000_000_000, End: 1_000_000_000},
		{ID: "b", Start: 0, End: 0},
	}
	res := Plan(ivs, nil)
	if len(res.Points) != 2 {
		t.Fatalf("plan = %+v", res.Points)
	}

	// Frozen [1,10^9] leaves 0 usable.
	frozen := []FrozenRange{{Start: 1, End: 500_000_000}, {Start: 500_000_001, End: 1_000_000_000}}
	ivs2 := []Interval{{ID: "x", Start: 0, End: 1_000_000_000}}
	res2 := Plan(ivs2, frozen)
	if len(res2.Points) != 1 || res2.Points[0].Batch != 0 {
		t.Fatalf("plan = %+v", res2.Points)
	}

	// Entire universe frozen => failure with no partial plan.
	res3 := Plan(ivs, append(frozen, FrozenRange{Start: 0, End: 0}))
	if res3.Status != "NO_SAMPLE_POINT" || len(res3.Points) != 0 {
		t.Fatalf("res3 = %+v", res3)
	}
	if res3.Fail.IntervalID != "b" || res3.Fail.Position != 1 {
		t.Fatalf("failure = %+v, order=%v", res3.Fail, res3.Order)
	}
}

func TestFailureEarliestInProcessingOrder(t *testing.T) {
	// 'later' has smaller right endpoint and fails although it appears later in
	// input; it must be reported as the earliest failure in processing order.
	ivs := []Interval{
		{ID: "first", Start: 8, End: 9},
		{ID: "later", Start: 2, End: 2},
		{ID: "other", Start: 0, End: 1},
	}
	frozen := []FrozenRange{{Start: 2, End: 2}}
	res := Plan(ivs, frozen)
	if res.Status != "NO_SAMPLE_POINT" {
		t.Fatalf("res = %+v", res)
	}
	// Order by end: other(1), later(2, frozen -> fails), first(9).
	if !reflect.DeepEqual(res.Order, []string{"other", "later", "first"}) {
		t.Fatalf("order = %v", res.Order)
	}
	if res.Fail.IntervalID != "later" || res.Fail.Position != 2 {
		t.Fatalf("fail = %+v", res.Fail)
	}
}
