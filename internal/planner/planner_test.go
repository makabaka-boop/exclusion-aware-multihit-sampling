package planner

import (
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// reqHits returns the effective hit requirement (unset means 1).
func reqHits(iv Interval) int {
	if iv.RequiredHits < 1 {
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
// gives every risk interval at least RequiredHits distinct hits. ok=false when
// some interval does not contain enough free integers.
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
		if cnt < reqHits(iv) {
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
			hit := 0
			for b, p := range points {
				if chosen[b] && p >= iv.Start && p <= iv.End {
					hit++
				}
			}
			if hit < reqHits(iv) {
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
// feasibility, per-interval hit counts, minimality against brute force,
// increasing batches, hit/coverage transposes, stable processing order and
// determinism.
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

	seg := NormalizeFrozen(frozen)
	min, feasible := bruteMin(intervals, frozen)

	if !feasible {
		if res.Status != "NO_SAMPLE_POINT" {
			t.Fatalf("want NO_SAMPLE_POINT, got %+v", res)
		}
		if len(res.Points) != 0 || len(res.Hits) != 0 {
			t.Fatalf("failure must not leave a partial plan, got %+v / %+v", res.Points, res.Hits)
		}
		// Failed interval must be the earliest (by processing order) one with
		// too few free integers, and the gap must match exactly.
		earliest := ""
		var pos, missing int
		for p, id := range res.Order {
			iv := byID[id]
			free := bruteFreeSet(seg, iv.Start, iv.End)
			if k := reqHits(iv); len(free) < k {
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
			t.Fatalf("failed missing = %d, want %d", res.Fail.Missing, missing)
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

	// Batches strictly increasing and distinct.
	for i := 1; i < len(res.Points); i++ {
		if res.Points[i-1].Batch >= res.Points[i].Batch {
			t.Fatalf("batches not strictly increasing: %+v", res.Points)
		}
	}

	// Every chosen batch is outside every frozen segment (normalized).
	for _, pt := range res.Points {
		for _, s := range seg {
			if pt.Batch >= s.Start && pt.Batch <= s.End {
				t.Fatalf("chosen batch %d lies in frozen segment %v", pt.Batch, s)
			}
		}
	}

	// Per-interval hits: exactly the selected batches inside the interval,
	// ascending, at least RequiredHits many, aligned with processing order.
	if len(res.Hits) != len(intervals) {
		t.Fatalf("hits length = %d, want %d", len(res.Hits), len(intervals))
	}
	coveredBy := map[int64][]string{}
	for pos, id := range res.Order {
		h := res.Hits[pos]
		if h.IntervalID != id {
			t.Fatalf("hits[%d].id = %q, want %q (processing order)", pos, h.IntervalID, id)
		}
		iv := byID[id]
		want := []int64{}
		for _, pt := range res.Points {
			if pt.Batch >= iv.Start && pt.Batch <= iv.End {
				want = append(want, pt.Batch)
			}
		}
		if !reflect.DeepEqual(h.Batches, want) {
			t.Fatalf("hits[%d] (%q) = %v, want %v", pos, id, h.Batches, want)
		}
		if len(h.Batches) < reqHits(iv) {
			t.Fatalf("interval %q has %d hits, requires %d", id, len(h.Batches), reqHits(iv))
		}
		for _, p := range h.Batches {
			coveredBy[p] = append(coveredBy[p], id)
		}
	}

	// Points' Covered lists are the transpose of the per-interval hits.
	for _, pt := range res.Points {
		if !reflect.DeepEqual(pt.Covered, coveredBy[pt.Batch]) {
			t.Fatalf("point %d covered = %v, want transpose %v", pt.Batch, pt.Covered, coveredBy[pt.Batch])
		}
		if len(pt.Covered) == 0 {
			t.Fatalf("point %d covers no interval", pt.Batch)
		}
	}
}

func TestNoFrozenClassic(t *testing.T) {
	// Classic greedy example, all intervals implicitly single-hit.
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

func TestRequiredHitsDefaultIsOne(t *testing.T) {
	// Unset RequiredHits must behave exactly like an explicit 1.
	ivs := []Interval{
		{ID: "a", Start: 0, End: 4},
		{ID: "b", Start: 1, End: 3},
	}
	explicit := []Interval{
		{ID: "a", Start: 0, End: 4, RequiredHits: 1},
		{ID: "b", Start: 1, End: 3, RequiredHits: 1},
	}
	if got, want := Plan(ivs, nil), Plan(explicit, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("unset != explicit 1:\n%+v\n%+v", got, want)
	}
	verifyResult(t, ivs, nil)
}

func TestMultiHitPicksRightToLeft(t *testing.T) {
	// One interval asking for 3 distinct samples gets the three rightmost
	// free integers.
	ivs := []Interval{{ID: "a", Start: 2, End: 9, RequiredHits: 3}}
	res := Plan(ivs, nil)
	want := []int64{7, 8, 9}
	got := []int64{res.Points[0].Batch, res.Points[1].Batch, res.Points[2].Batch}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(res.Hits[0].Batches, want) {
		t.Fatalf("hits = %v, want %v", res.Hits[0].Batches, want)
	}
	verifyResult(t, ivs, nil)
}

func TestOverlappingRequiredHits(t *testing.T) {
	// Overlapping demands share batches: [1,5]x2 and [3,7]x2 need only
	// {4,5}: both batches lie in both intervals.
	ivs := []Interval{
		{ID: "a", Start: 1, End: 5, RequiredHits: 2},
		{ID: "b", Start: 3, End: 7, RequiredHits: 2},
	}
	res := Plan(ivs, nil)
	if len(res.Points) != 2 || res.Points[0].Batch != 4 || res.Points[1].Batch != 5 {
		t.Fatalf("plan = %+v", res.Points)
	}
	for _, h := range res.Hits {
		if !reflect.DeepEqual(h.Batches, []int64{4, 5}) {
			t.Fatalf("hits of %q = %v", h.IntervalID, h.Batches)
		}
	}
	for _, pt := range res.Points {
		if !reflect.DeepEqual(pt.Covered, []string{"a", "b"}) {
			t.Fatalf("point %d covered = %v", pt.Batch, pt.Covered)
		}
	}
	verifyResult(t, ivs, nil)

	// A later point falling inside an already-satisfied interval still shows
	// up in that interval's actual hits.
	ivs2 := []Interval{
		{ID: "i", Start: 8, End: 10},
		{ID: "j", Start: 1, End: 10, RequiredHits: 2},
	}
	res2 := Plan(ivs2, nil)
	if len(res2.Points) != 2 || res2.Points[0].Batch != 9 || res2.Points[1].Batch != 10 {
		t.Fatalf("plan2 = %+v", res2.Points)
	}
	// Processing order is i (end 10, id i) then j; i is actually hit by both.
	if !reflect.DeepEqual(res2.Hits[0].Batches, []int64{9, 10}) ||
		!reflect.DeepEqual(res2.Hits[1].Batches, []int64{9, 10}) {
		t.Fatalf("hits2 = %+v", res2.Hits)
	}
	verifyResult(t, ivs2, nil)
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

func TestTouchingFrozenMultiHit(t *testing.T) {
	// Touching frozen segments merge, so [0,4] keeps only {0,4} selectable
	// and a demand of 3 fails with a gap of 1.
	ivs := []Interval{{ID: "a", Start: 0, End: 4, RequiredHits: 3}}
	frozen := []FrozenRange{{Start: 1, End: 2}, {Start: 3, End: 3}}
	res := Plan(ivs, frozen)
	if res.Status != "NO_SAMPLE_POINT" {
		t.Fatalf("res = %+v", res)
	}
	if res.Fail.IntervalID != "a" || res.Fail.Position != 1 || res.Fail.Missing != 1 {
		t.Fatalf("fail = %+v", res.Fail)
	}
	if len(res.Points) != 0 || len(res.Hits) != 0 {
		t.Fatalf("partial plan leaked: %+v", res)
	}
	verifyResult(t, ivs, frozen)

	// Demand 2 fits exactly: the two endpoints survive.
	ivs2 := []Interval{{ID: "a", Start: 0, End: 4, RequiredHits: 2}}
	res2 := Plan(ivs2, frozen)
	if len(res2.Points) != 2 || res2.Points[0].Batch != 0 || res2.Points[1].Batch != 4 {
		t.Fatalf("plan = %+v", res2.Points)
	}
	verifyResult(t, ivs2, frozen)
}

func TestSinglePointIntervals(t *testing.T) {
	cases := []struct {
		name    string
		ivs     []Interval
		frozen  []FrozenRange
		failID  string
		missing int
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
			// [5,5] has zero free integers and needs 1.
			missing: 1,
		},
		{
			name:   "whole endpoint range frozen by touching pair",
			ivs:    []Interval{{ID: "a", Start: 4, End: 5}},
			frozen: []FrozenRange{{Start: 4, End: 4}, {Start: 5, End: 5}},
			failID: "a",
			// [4,5] has zero free integers and needs 1.
			missing: 1,
		},
		{
			name:    "single point cannot serve two samples",
			ivs:     []Interval{{ID: "a", Start: 9, End: 9, RequiredHits: 2}},
			failID:  "a",
			missing: 1,
		},
		{
			name:    "single point interval demands three",
			ivs:     []Interval{{ID: "a", Start: 9, End: 9, RequiredHits: 3}},
			failID:  "a",
			missing: 2,
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
				if res.Fail.Missing != tc.missing {
					t.Fatalf("missing = %d, want %d", res.Fail.Missing, tc.missing)
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

	// Same right endpoint with distinct demands: the single-hit interval is
	// processed first (id order), then the double-hit one tops up to its left.
	ivs3 := []Interval{
		{ID: "b", Start: 0, End: 5, RequiredHits: 2},
		{ID: "a", Start: 0, End: 5},
	}
	res3 := Plan(ivs3, nil)
	if !reflect.DeepEqual(res3.Order, []string{"a", "b"}) {
		t.Fatalf("order = %v", res3.Order)
	}
	if len(res3.Points) != 2 || res3.Points[0].Batch != 4 || res3.Points[1].Batch != 5 {
		t.Fatalf("plan = %+v", res3.Points)
	}
	verifyResult(t, ivs3, nil)
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

func TestFrozenMultiHitSkipsUsedAndFrozen(t *testing.T) {
	// [0,6] needs 3 distinct batches; 6 and 5 are frozen, so the picks are
	// 4, 3, 2 from right to left.
	ivs := []Interval{{ID: "a", Start: 0, End: 6, RequiredHits: 3}}
	frozen := []FrozenRange{{Start: 5, End: 6}}
	res := Plan(ivs, frozen)
	want := []int64{2, 3, 4}
	got := []int64{res.Points[0].Batch, res.Points[1].Batch, res.Points[2].Batch}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	verifyResult(t, ivs, frozen)
}

// itoaTest keeps the exhaustive loop free of strconv noise at call sites.
func itoaTest(n int) string { return strconv.Itoa(n) }

// TestExhaustiveSmall enumerates all combinations of risk intervals over a
// small coordinate universe, a spread of RequiredHits assignments and every
// frozen mask, cross-checking the greedy result against brute-force
// enumeration of all point subsets.
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

	// Hit-demand patterns: exhaustive for singles and pairs, representative
	// (uniform and mixed) for triples to keep the runtime bounded.
	pat1 := [][1]int{{1}, {2}, {3}}
	pat2 := [][2]int{{1, 1}, {1, 2}, {1, 3}, {2, 1}, {2, 2}, {2, 3}, {3, 1}, {3, 2}, {3, 3}}
	pat3 := [][3]int{{1, 1, 1}, {2, 2, 2}, {3, 3, 3}, {1, 2, 3}, {3, 2, 1}, {2, 1, 3}}

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

		emit := func(ks []int, picks ...raw) {
			ivs := make([]Interval, len(picks))
			for k, p := range picks {
				ivs[k] = Interval{ID: "i" + itoaTest(k), Start: p.l, End: p.r, RequiredHits: ks[k]}
			}
			verifyResult(t, ivs, frozen)
			cases++
		}
		for i := 0; i < len(all); i++ {
			for _, p1 := range pat1 {
				emit([]int{p1[0]}, all[i])
			}
			for j := i; j < len(all); j++ {
				for _, p2 := range pat2 {
					emit([]int{p2[0], p2[1]}, all[i], all[j])
				}
				for k := j; k < len(all); k++ {
					for _, p3 := range pat3 {
						emit([]int{p3[0], p3[1], p3[2]}, all[i], all[j], all[k])
					}
				}
			}
		}
	}
	t.Logf("exhaustively verified %d small cases", cases)
}

// TestRandomizedLargeCoordinates fuzzes realistic instances including large
// coordinates (where brute force enumerates only the free points that actually
// matter), overlapping demands, touching segments and fully-frozen failures.
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
			ivs[k] = Interval{ID: id, Start: l, End: r, RequiredHits: 1 + rng.Intn(3)}
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

	// Three distinct samples at the top of the range: picked right to left.
	ivs4 := []Interval{{ID: "top", Start: 999_999_998, End: 1_000_000_000, RequiredHits: 3}}
	res4 := Plan(ivs4, nil)
	want := []int64{999_999_998, 999_999_999, 1_000_000_000}
	for i, w := range want {
		if res4.Points[i].Batch != w {
			t.Fatalf("point %d = %d, want %d", i, res4.Points[i].Batch, w)
		}
	}
	if !reflect.DeepEqual(res4.Hits[0].Batches, want) {
		t.Fatalf("hits = %v, want %v", res4.Hits[0].Batches, want)
	}

	// A two-integer island at 10^9 cannot supply 3 samples: gap of 1.
	frozen5 := []FrozenRange{{Start: 0, End: 999_999_998}}
	res5 := Plan(ivs4, frozen5)
	if res5.Status != "NO_SAMPLE_POINT" || res5.Fail.Missing != 1 || res5.Fail.IntervalID != "top" {
		t.Fatalf("res5 = %+v", res5)
	}
	if len(res5.Points) != 0 || len(res5.Hits) != 0 {
		t.Fatalf("partial plan leaked: %+v", res5)
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
