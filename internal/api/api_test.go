package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func post(t *testing.T, h http.Handler, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sample-plan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	raw, _ := io.ReadAll(rec.Result().Body)
	var parsed map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("invalid JSON response %q: %v", string(raw), err)
		}
	}
	return rec.Code, parsed
}

func TestAPI_HappyPath(t *testing.T) {
	h := Handler()
	body := `{
		"intervals": [
			{"id": "R1", "start": 1, "end": 5},
			{"id": "R2", "start": 6, "end": 10}
		],
		"frozen": [{"start": 5, "end": 5}]
	}`
	status, got := post(t, h, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, got)
	}
	if got["status"] != "OK" {
		t.Fatalf("status field = %v", got["status"])
	}
	// R1 -> 4 (5 frozen), R2 -> 10.
	points := got["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("points = %v", points)
	}
	first := points[0].(map[string]any)
	if int64(first["batch"].(float64)) != 4 {
		t.Fatalf("first batch = %v", first["batch"])
	}
	if !reflect.DeepEqual(asStrings(first["covered_ids"].([]any)), []string{"R1"}) {
		t.Fatalf("covered = %v", first["covered_ids"])
	}
	if !reflect.DeepEqual(asStrings(got["processing_order"].([]any)), []string{"R1", "R2"}) {
		t.Fatalf("order = %v", got["processing_order"])
	}
	if int(got["sample_count"].(float64)) != 2 {
		t.Fatalf("sample_count = %v", got["sample_count"])
	}
	// Unset requiredHits keeps single-hit semantics; per-interval hits follow
	// the processing order.
	hits := got["interval_hits"].([]any)
	if len(hits) != 2 {
		t.Fatalf("interval_hits = %v", hits)
	}
	for i, want := range []struct {
		id      string
		batches []int64
	}{{"R1", []int64{4}}, {"R2", []int64{10}}} {
		h := hits[i].(map[string]any)
		if h["interval_id"] != want.id || !reflect.DeepEqual(asInt64s(h["batches"].([]any)), want.batches) {
			t.Fatalf("interval_hits[%d] = %v, want %v", i, h, want)
		}
	}
}

func TestAPI_RequiredHits(t *testing.T) {
	h := Handler()
	// R1 needs two distinct batches inside [1,5]; R2 shares them since [3,5]
	// also contains 4 and 5.
	body := `{
		"intervals": [
			{"id": "R1", "start": 1, "end": 5, "requiredHits": 2},
			{"id": "R2", "start": 3, "end": 5}
		],
		"frozen": []
	}`
	status, got := post(t, h, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, got)
	}
	if int(got["sample_count"].(float64)) != 2 {
		t.Fatalf("sample_count = %v", got["sample_count"])
	}
	hits := got["interval_hits"].([]any)
	for i, wantID := range []string{"R1", "R2"} {
		h := hits[i].(map[string]any)
		if h["interval_id"] != wantID {
			t.Fatalf("hits[%d] = %v", i, h)
		}
		if !reflect.DeepEqual(asInt64s(h["batches"].([]any)), []int64{4, 5}) {
			t.Fatalf("hits[%d] batches = %v", i, h["batches"])
		}
	}
}

func TestAPI_NoSamplePoint(t *testing.T) {
	h := Handler()
	body := `{
		"intervals": [{"id": "R1", "start": 3, "end": 4}],
		"frozen": [{"start": 3, "end": 3}, {"start": 4, "end": 4}]
	}`
	status, got := post(t, h, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got["status"] != "NO_SAMPLE_POINT" {
		t.Fatalf("body = %v", got)
	}
	if got["failed_interval_id"] != "R1" || int(got["failed_position"].(float64)) != 1 {
		t.Fatalf("failure fields = %v", got)
	}
	// [3,4] is fully frozen: zero free integers for a demand of one -> gap 1.
	if int(got["failed_missing"].(float64)) != 1 {
		t.Fatalf("failed_missing = %v", got["failed_missing"])
	}
	if int(got["sample_count"].(float64)) != 0 {
		t.Fatalf("sample_count = %v", got["sample_count"])
	}
	if len(got["points"].([]any)) != 0 {
		t.Fatalf("partial plan leaked: %v", got["points"])
	}
}

func TestAPI_RequiredHitsGap(t *testing.T) {
	h := Handler()
	// [1,3] has exactly two free integers (1 and 2) for a demand of 3: gap 1.
	body := `{
		"intervals": [{"id": "R1", "start": 1, "end": 3, "requiredHits": 3}],
		"frozen": [{"start": 3, "end": 3}]
	}`
	status, got := post(t, h, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, got)
	}
	if got["status"] != "NO_SAMPLE_POINT" || got["failed_interval_id"] != "R1" {
		t.Fatalf("body = %v", got)
	}
	if int(got["failed_position"].(float64)) != 1 || int(got["failed_missing"].(float64)) != 1 {
		t.Fatalf("failure fields = %v", got)
	}
	if len(got["points"].([]any)) != 0 {
		t.Fatalf("partial plan leaked: %v", got["points"])
	}
}

func TestAPI_Validation422(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"unknown top field":        `{"intervals":[],"frozen":[],"x":1}`,
		"unknown nested field":     `{"intervals":[{"id":"a","start":0,"end":1,"z":2}],"frozen":[]}`,
		"empty intervals":          `{"intervals":[],"frozen":[]}`,
		"missing intervals":        `{"frozen":[]}`,
		"missing frozen":           `{"intervals":[{"id":"a","start":0,"end":1}]}`,
		"missing id":               `{"intervals":[{"start":0,"end":1}],"frozen":[]}`,
		"missing start":            `{"intervals":[{"id":"a","end":1}],"frozen":[]}`,
		"missing end":              `{"intervals":[{"id":"a","start":0}],"frozen":[]}`,
		"duplicate id":             `{"intervals":[{"id":"a","start":0,"end":1},{"id":"a","start":2,"end":3}],"frozen":[]}`,
		"start greater than end":   `{"intervals":[{"id":"a","start":5,"end":1}],"frozen":[]}`,
		"negative endpoint":        `{"intervals":[{"id":"a","start":-1,"end":1}],"frozen":[]}`,
		"endpoint above 1e9":       `{"intervals":[{"id":"a","start":0,"end":1000000001}],"frozen":[]}`,
		"float endpoint":           `{"intervals":[{"id":"a","start":0.5,"end":1}],"frozen":[]}`,
		"string endpoint":          `{"intervals":[{"id":"a","start":"0","end":1}],"frozen":[]}`,
		"null body":                `null`,
		"array body":               `[]`,
		"two json values":          `{"intervals":[{"id":"a","start":0,"end":1}],"frozen":[]}{}`,
		"frozen missing start":     `{"intervals":[{"id":"a","start":0,"end":1}],"frozen":[{"end":5}]}`,
		"frozen start greater end": `{"intervals":[{"id":"a","start":0,"end":1}],"frozen":[{"start":9,"end":1}]}`,
		"frozen negative":          `{"intervals":[{"id":"a","start":0,"end":1}],"frozen":[{"start":-2,"end":-1}]}`,
		"empty id":                 `{"intervals":[{"id":"","start":0,"end":1}],"frozen":[]}`,
		"malformed json":           `{not json`,
		"requiredHits zero":        `{"intervals":[{"id":"a","start":0,"end":1,"requiredHits":0}],"frozen":[]}`,
		"requiredHits above three": `{"intervals":[{"id":"a","start":0,"end":1,"requiredHits":4}],"frozen":[]}`,
		"requiredHits negative":    `{"intervals":[{"id":"a","start":0,"end":1,"requiredHits":-1}],"frozen":[]}`,
		"requiredHits float":       `{"intervals":[{"id":"a","start":0,"end":1,"requiredHits":1.5}],"frozen":[]}`,
		"requiredHits string":      `{"intervals":[{"id":"a","start":0,"end":1,"requiredHits":"2"}],"frozen":[]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, got := post(t, h, body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422, body=%v", status, got)
			}
			if got["error"] == nil || got["error"] == "" {
				t.Fatalf("missing error message: %v", got)
			}
		})
	}
}

func TestAPI_Limits(t *testing.T) {
	h := Handler()

	// 5001 intervals -> 422.
	var sb strings.Builder
	sb.WriteString(`{"intervals":[`)
	for i := 0; i < 5001; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"i`)
		sb.WriteString(itoa(i))
		sb.WriteString(`","start":0,"end":1000000000}`)
	}
	sb.WriteString(`],"frozen":[]}`)
	if status, got := post(t, h, sb.String()); status != http.StatusUnprocessableEntity {
		t.Fatalf("5001 intervals: status=%d body=%v", status, got)
	}

	// Exactly 5000 intervals -> 200 with a single shared sample.
	sb.Reset()
	sb.WriteString(`{"intervals":[`)
	for i := 0; i < 5000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"i`)
		sb.WriteString(itoa(i))
		sb.WriteString(`","start":0,"end":1000000000}`)
	}
	sb.WriteString(`],"frozen":[]}`)
	status, got := post(t, h, sb.String())
	if status != http.StatusOK {
		t.Fatalf("5000 intervals: status=%d body=%v", status, got)
	}
	if int(got["sample_count"].(float64)) != 1 {
		t.Fatalf("sample_count = %v", got["sample_count"])
	}
	if len(got["processing_order"].([]any)) != 5000 {
		t.Fatalf("order length = %d", len(got["processing_order"].([]any)))
	}
}

func TestAPI_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/sample-plan", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Allow") != "POST" {
		t.Fatalf("Allow header = %q", rec.Header().Get("Allow"))
	}
}

func asStrings(in []any) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = v.(string)
	}
	return out
}

func asInt64s(in []any) []int64 {
	out := make([]int64, len(in))
	for i, v := range in {
		out[i] = int64(v.(float64))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
