package planner

import (
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"testing"
)

// hitsOf normalizes an interval's demand: an unset RequiredHits means one hit.
func hitsOf(iv Interval) int {
	if iv.RequiredHits <= 0 {
		return 1
	}
	return iv.RequiredHits
}

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
// gives every risk interval its RequiredHits distinct hits. ok=false when
// some interval does not contain enough usable integers.
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
		cnt := 0
		for x := iv.Start; x <= iv.End; x++ {
			if free[x] {
				cnt++
			}
		}
		if cnt < hitsOf(iv) {
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
			hits := 0
			for b, p := range points {
				if chosen[b] && p >= iv.Start && p <= iv.End {
					hits++
				}
			}
			if hits < hitsOf(iv) {
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
// feasibility, per-interval hit demands, minimality against brute force,
// increasing batches, point/interval coverage consistency, stable processing
// order, failure gap and determinism.
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
		if len(res.Points) != 0 || len(res.Hits) != 0 {
			t.Fatalf("failure must not leave a partial plan, got points=%+v hits=%+v", res.Points, res.Hits)
		}
		// The failed interval must be the earliest (by processing order)
		// whose usable integers fall short of its demand, and Missing must
		// be the exact gap.
		seg := NormalizeFrozen(frozen)
		earliest := ""
		var pos, missing int
		for p, id := range res.Order {
			iv := byID[id]
			free := bruteFreeSet(seg, iv.Start, iv.End)
			if k := hitsOf(iv); len(free) < k {
				earliest, pos, missing = id, p+1, k-len(free)
				break
			}
		}
		if res.Fail.IntervalID != earliest {
			t.Fatalf("failed interval = %q, want earliest %q", res.Fail.IntervalID, earliest)
		}
		if res.Fail.Position != pos {
			t.Fatalf("failed position = %d, want %d", res.Fail.Position, pos)
		}
		if res.Fail.Missing != missing {
			t.Fatalf("missing = %d, want %d", res.Fail.Missing, missing)
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
	selected := map[int64]bool{}
	for _, pt := range res.Points {
		selected[pt.Batch] = true
		for _, s := range seg {
			if pt.Batch >= s.Start && pt.Batch <= s.End {
				t.Fatalf("chosen batch %d lies in frozen segment %v", pt.Batch, s)
			}
		}
	}

	// Per-interval hits: reported in processing order, exactly RequiredHits
	// distinct selected batches inside the interval.
	if len(res.Hits) != len(intervals) {
		t.Fatalf("hits length = %d, want %d", len(res.Hits), len(intervals))
	}
	hitByID := map[string][]int64{}
	for pos, h := range res.Hits {
		if h.ID != res.Order[pos] {
			t.Fatalf("hits[%d] = %q, not in processing order %v", pos, h.ID, res.Order)
		}
		iv := byID[h.ID]
		if len(h.Batches) != hitsOf(iv) {
			t.Fatalf("interval %q has %d hits, want %d", h.ID, len(h.Batches), hitsOf(iv))
		}
		for i, b := range h.Batches {
			if i > 0 && h.Batches[i-1] >= b {
				t.Fatalf("hits of %q not strictly increasing: %v", h.ID, h.Batches)
			}
			if !selected[b] {
				t.Fatalf("hit batch %d of %q was not selected", b, h.ID)
			}
			if b < iv.Start || b > iv.End {
				t.Fatalf("hit batch %d outside interval %q = %v", b, h.ID, iv)
			}
		}
		hitByID[h.ID] = h.Batches
	}

	// Points' covered lists must be the exact transpose of the per-interval
	// hits, in processing order, and every interval must be assigned exactly
	// RequiredHits times.
	posOf := map[string]int{}
	for pos, id := range res.Order {
		posOf[id] = pos
	}
	assigned := map[string][]int64{}
	for _, pt := range res.Points {
		prev := -1
		for _, id := range pt.Covered {
			if posOf[id] <= prev {
				t.Fatalf("covered list of batch %d not in processing order: %v", pt.Batch, pt.Covered)
			}
			prev = posOf[id]
			assigned[id] = append(assigned[id], pt.Batch)
		}
	}
	for id, batches := range assigned {
		slices.Sort(batches)
		if !reflect.DeepEqual(batches, hitByID[id]) {
			t.Fatalf("interval %q covered by %v but hits say %v", id, batches, hitByID[id])
		}
	}
	if len(assigned) != len(intervals) {
		t.Fatalf("covered lists mention %d intervals, want %d", len(assigned), len(intervals))
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

func TestRequiredHitsDefaultsToOne(t *testing.T) {
	// An unset RequiredHits keeps the legacy single-hit semantics exactly.
	ivs := []Interval{
		{ID: "a", Start: 1, End: 4},
		{ID: "b", Start: 2, End: 5},
		{ID: "c", Start: 4, End: 4},
	}
	withOne := make([]Interval, len(ivs))
	for i, iv := range ivs {
		iv.RequiredHits = 1
		withOne[i] = iv
	}
	if got, want := Plan(ivs, nil), Plan(withOne, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("unset RequiredHits differs from 1:\n%+v\n%+v", got, want)
	}
}

func TestOverlappingRequiredHits(t *testing.T) {
	// Overlapping demands share the same physical batches: one selected
	// batch serves several intervals at once.
	ivs := []Interval{
		{ID: "a", Start: 1, End: 6, RequiredHits: 2},
		{ID: "b", Start: 3, End: 9, RequiredHits: 3},
		{ID: "c", Start: 5, End: 7, RequiredHits: 2},
	}
	verifyResult(t, ivs, nil)

	res := Plan(ivs, nil)
	// Processing order: a(6), c(7), b(9). a picks 6,5; c is satisfied by
	// {5,6}; b still needs one more and picks 9.
	if !reflect.DeepEqual(res.Order, []string{"a", "c", "b"}) {
		t.Fatalf("order = %v", res.Order)
	}
	var batches []int64
	for _, p := range res.Points {
		batches = append(batches, p.Batch)
	}
	if !reflect.DeepEqual(batches, []int64{5, 6, 9}) {
		t.Fatalf("batches = %v", batches)
	}
	wantHits := []IntervalHits{
		{ID: "a", Batches: []int64{5, 6}},
		{ID: "c", Batches: []int64{5, 6}},
		{ID: "b", Batches: []int64{5, 6, 9}},
	}
	if !reflect.DeepEqual(res.Hits, wantHits) {
		t.Fatalf("hits = %+v", res.Hits)
	}
}

func TestAdjacentFrozenSegmentsRequiredHits(t *testing.T) {
	// [2,3] and [4,5] touch, so they merge into [2,5] and leave exactly
	// {1,6,7} usable inside [1,7].
	frozen := []FrozenRange{{Start: 2, End: 3}, {Start: 4, End: 5}}
	ivs := []Interval{{ID: "a", Start: 1, End: 7, RequiredHits: 3}}
	verifyResult(t, ivs, frozen)

	res := Plan(ivs, frozen)
	var batches []int64
	for _, p := range res.Points {
		batches = append(batches, p.Batch)
	}
	if !reflect.DeepEqual(batches, []int64{1, 6, 7}) {
		t.Fatalf("batches = %v", batches)
	}

	// One integer fewer in the window and the demand falls short by one.
	ivs2 := []Interval{{ID: "a", Start: 1, End: 6, RequiredHits: 3}}
	verifyResult(t, ivs2, frozen)
	res2 := Plan(ivs2, frozen)
	if res2.Status != "NO_SAMPLE_POINT" || res2.Fail.Missing != 1 {
		t.Fatalf("res2 = %+v", res2)
	}
}

func TestSinglePointIntervalRequiredHits(t *testing.T) {
	// A single-point interval cannot provide two independent samples.
	res := Plan([]Interval{{ID: "s", Start: 7, End: 7, RequiredHits: 2}}, nil)
	if res.Status != "NO_SAMPLE_POINT" || res.Fail.Missing != 1 {
		t.Fatalf("res = %+v", res)
	}

	// Frozen single point: the gap equals the full demand.
	res2 := Plan([]Interval{{ID: "s", Start: 7, End: 7, RequiredHits: 3}}, []FrozenRange{{Start: 7, End: 7}})
	if res2.Status != "NO_SAMPLE_POINT" || res2.Fail.Missing != 3 {
		t.Fatalf("res2 = %+v", res2)
	}

	// Two single-point intervals at the same coordinate share one sample.
	ivs := []Interval{
		{ID: "s1", Start: 7, End: 7, RequiredHits: 1},
		{ID: "s2", Start: 7, End: 7, RequiredHits: 1},
	}
	verifyResult(t, ivs, nil)
	res3 := Plan(ivs, nil)
	if len(res3.Points) != 1 || res3.Points[0].Batch != 7 {
		t.Fatalf("res3 = %+v", res3.Points)
	}
}

func TestRequiredHitsGapAfterPartialPicks(t *testing.T) {
	// a seeds one batch inside b's window; b still falls one usable integer
	// short, and the gap must reflect what remains after the partial picks.
	ivs := []Interval{
		{ID: "a", Start: 1, End: 4, RequiredHits: 1},
		{ID: "b", Start: 3, End: 5, RequiredHits: 3},
	}
	frozen := []FrozenRange{{Start: 5, End: 5}}
	verifyResult(t, ivs, frozen)

	res := Plan(ivs, frozen)
	if res.Status != "NO_SAMPLE_POINT" {
		t.Fatalf("res = %+v", res)
	}
	if res.Fail.IntervalID != "b" || res.Fail.Position != 2 || res.Fail.Missing != 1 {
		t.Fatalf("fail = %+v", res.Fail)
	}
}

func TestFailurePublishesNoPartialPlan(t *testing.T) {
	// The first interval succeeds, the second fails: nothing of the partial
	// plan may leak into the result.
	ivs := []Interval{
		{ID: "ok", Start: 1, End: 3, RequiredHits: 2},
		{ID: "bad", Start: 10, End: 11, RequiredHits: 3},
	}
	res := Plan(ivs, nil)
	if res.Status != "NO_SAMPLE_POINT" {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Points) != 0 || len(res.Hits) != 0 {
		t.Fatalf("partial plan leaked: points=%+v hits=%+v", res.Points, res.Hits)
	}
	if res.Fail.IntervalID != "bad" || res.Fail.Position != 2 || res.Fail.Missing != 1 {
		t.Fatalf("fail = %+v", res.Fail)
	}
	verifyResult(t, ivs, nil)
}

func TestBillionScaleRequiredHits(t *testing.T) {
	// Three independent samples at the 10^9 boundary.
	ivs := []Interval{{ID: "big", Start: 0, End: 1_000_000_000, RequiredHits: 3}}
	res := Plan(ivs, nil)
	var batches []int64
	for _, p := range res.Points {
		batches = append(batches, p.Batch)
	}
	if !reflect.DeepEqual(batches, []int64{999_999_998, 999_999_999, 1_000_000_000}) {
		t.Fatalf("batches = %v", batches)
	}

	// A frozen tail pushes the three samples across a huge gap in one jump.
	frozenTail := []FrozenRange{{Start: 10, End: 1_000_000_000}}
	res2 := Plan(ivs, frozenTail)
	batches = batches[:0]
	for _, p := range res2.Points {
		batches = append(batches, p.Batch)
	}
	if !reflect.DeepEqual(batches, []int64{7, 8, 9}) {
		t.Fatalf("batches = %v", batches)
	}

	// A huge frozen middle leaves {0..4} and 10^9 usable.
	frozenMid := []FrozenRange{{Start: 5, End: 999_999_999}}
	res3 := Plan(ivs, frozenMid)
	batches = batches[:0]
	for _, p := range res3.Points {
		batches = append(batches, p.Batch)
	}
	if !reflect.DeepEqual(batches, []int64{3, 4, 1_000_000_000}) {
		t.Fatalf("batches = %v", batches)
	}

	// Only {0,1} usable against a demand of 3: gap of 1, no partial plan.
	frozenAll := []FrozenRange{{Start: 2, End: 1_000_000_000}}
	res4 := Plan(ivs, frozenAll)
	if res4.Status != "NO_SAMPLE_POINT" || res4.Fail.Missing != 1 || len(res4.Points) != 0 {
		t.Fatalf("res4 = %+v", res4)
	}
}

// itoaTest keeps the exhaustive loop free of strconv noise at call sites.
func itoaTest(n int) string { return strconv.Itoa(n) }

// TestExhaustiveSmall enumerates all combinations of risk intervals over a
// small coordinate universe, every frozen mask and every RequiredHits
// assignment in {1,2,3}, cross-checking the greedy result against
// brute-force enumeration of all point subsets.
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

		// Enumerate every singleton/pair/triple of intervals with every
		// RequiredHits assignment in {1,2,3} for every frozen mask:
		// 15*3 + 120*9 + 680*27 = 19485 instances per mask. Each brute
		// force scans at most 2^5 subsets, so this is cheap.
		emit := func(picks []raw, ks []int) {
			ivs := make([]Interval, len(picks))
			for i, p := range picks {
				ivs[i] = Interval{ID: "i" + itoaTest(i), Start: p.l, End: p.r, RequiredHits: ks[i]}
			}
			verifyResult(t, ivs, frozen)
			cases++
		}
		ks := []int{1, 2, 3}
		for i := 0; i < len(all); i++ {
			for _, k1 := range ks {
				emit([]raw{all[i]}, []int{k1})
			}
			for j := i; j < len(all); j++ {
				for _, k1 := range ks {
					for _, k2 := range ks {
						emit([]raw{all[i], all[j]}, []int{k1, k2})
					}
				}
				for k := j; k < len(all); k++ {
					for _, k1 := range ks {
						for _, k2 := range ks {
							for _, k3 := range ks {
								emit([]raw{all[i], all[j], all[k]}, []int{k1, k2, k3})
							}
						}
					}
				}
			}
		}
	}
	t.Logf("exhaustively verified %d small cases", cases)
}

// TestRandomizedLargeCoordinates fuzzes realistic instances including large
// coordinates (where brute force enumerates only the free points that actually
// matter), overlaps, RequiredHits between 0 and 3, touching segments and
// short-of-usable failures.
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
			ivs[k] = Interval{ID: id, Start: l, End: r, RequiredHits: rng.Intn(4)}
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
	if res3.Fail.IntervalID != "b" || res3.Fail.Position != 1 || res3.Fail.Missing != 1 {
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
	if res.Fail.IntervalID != "later" || res.Fail.Position != 2 || res.Fail.Missing != 1 {
		t.Fatalf("fail = %+v", res.Fail)
	}
}
