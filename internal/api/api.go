// Package api wires the planner to an HTTP JSON endpoint.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"materiallab/internal/planner"
)

const (
	maxIntervals = 5000
	maxFrozen    = 5000
	maxEndpoint  = 1_000_000_000
)

// intervalDTO is the on-the-wire risk interval. Pointers distinguish a
// missing field from an explicit zero.
type intervalDTO struct {
	ID    *string `json:"id"`
	Start *int64  `json:"start"`
	End   *int64  `json:"end"`
	// RequiredHits is optional; a missing field keeps the legacy
	// single-hit semantics.
	RequiredHits *int64 `json:"required_hits"`
}

// frozenDTO is the on-the-wire frozen segment.
type frozenDTO struct {
	Start *int64 `json:"start"`
	End   *int64 `json:"end"`
}

type request struct {
	Intervals *[]intervalDTO `json:"intervals"`
	Frozen    *[]frozenDTO   `json:"frozen"`
}

type pointJSON struct {
	Batch      int64    `json:"batch"`
	CoveredIDs []string `json:"covered_ids"`
}

type intervalHitsJSON struct {
	ID      string  `json:"id"`
	Batches []int64 `json:"batches"`
}

type responseOK struct {
	Status          string             `json:"status"`
	SampleCount     int                `json:"sample_count"`
	Points          []pointJSON        `json:"points"`
	IntervalHits    []intervalHitsJSON `json:"interval_hits"`
	ProcessingOrder []string           `json:"processing_order"`
}

type responseFail struct {
	Status          string      `json:"status"`
	SampleCount     int         `json:"sample_count"`
	Points          []pointJSON `json:"points"`
	ProcessingOrder []string    `json:"processing_order"`
	FailedInterval  string      `json:"failed_interval_id"`
	FailedPosition  int         `json:"failed_position"`
	Missing         int         `json:"missing"`
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: msg})
}

// parseRequest strictly decodes and validates the body. Any structural or
// semantic violation results in an error and thus an HTTP 422.
func parseRequest(body []byte) ([]planner.Interval, []planner.FrozenRange, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var req request
	if err := dec.Decode(&req); err != nil {
		return nil, nil, err
	}
	// Reject trailing tokens / multiple JSON values.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, nil, errors.New("body must contain a single JSON object")
		}
		return nil, nil, err
	}

	if req.Intervals == nil {
		return nil, nil, errors.New("missing required field: intervals")
	}
	if req.Frozen == nil {
		return nil, nil, errors.New("missing required field: frozen")
	}

	raw := *req.Intervals
	if len(raw) < 1 || len(raw) > maxIntervals {
		return nil, nil, errors.New("intervals must contain between 1 and 5000 entries")
	}
	frz := *req.Frozen
	if len(frz) > maxFrozen {
		return nil, nil, errors.New("frozen must contain at most 5000 entries")
	}

	intervals := make([]planner.Interval, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, iv := range raw {
		if iv.ID == nil || iv.Start == nil || iv.End == nil {
			return nil, nil, errors.New("each interval requires id, start and end")
		}
		id := *iv.ID
		if id == "" {
			return nil, nil, errors.New("interval id must be a non-empty string")
		}
		if _, dup := seen[id]; dup {
			return nil, nil, errors.New("duplicate interval id: " + id)
		}
		seen[id] = struct{}{}
		if *iv.Start < 0 || *iv.Start > maxEndpoint ||
			*iv.End < 0 || *iv.End > maxEndpoint {
			return nil, nil, errors.New("endpoints must be integers in [0, 1000000000]")
		}
		if *iv.Start > *iv.End {
			return nil, nil, errors.New("interval start must be <= end")
		}
		requiredHits := 1
		if iv.RequiredHits != nil {
			if *iv.RequiredHits < 1 || *iv.RequiredHits > 3 {
				return nil, nil, errors.New("required_hits must be an integer in [1, 3]")
			}
			requiredHits = int(*iv.RequiredHits)
		}
		intervals = append(intervals, planner.Interval{
			ID:           id,
			Start:        *iv.Start,
			End:          *iv.End,
			RequiredHits: requiredHits,
		})
	}

	frozen := make([]planner.FrozenRange, 0, len(frz))
	for _, f := range frz {
		if f.Start == nil || f.End == nil {
			return nil, nil, errors.New("each frozen range requires start and end")
		}
		if *f.Start < 0 || *f.Start > maxEndpoint ||
			*f.End < 0 || *f.End > maxEndpoint {
			return nil, nil, errors.New("endpoints must be integers in [0, 1000000000]")
		}
		if *f.Start > *f.End {
			return nil, nil, errors.New("frozen start must be <= end")
		}
		frozen = append(frozen, planner.FrozenRange{Start: *f.Start, End: *f.End})
	}

	return intervals, frozen, nil
}

// Handler returns the service's HTTP handler.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sample-plan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
			return
		}

		// 1 MiB is far above the largest legal request (~1 MiB boundary for
		// 5000+5000 entries with long ids; cap generously at 2 MiB).
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, "request body too large or unreadable")
			return
		}

		intervals, frozen, err := parseRequest(body)
		if err != nil {
			writeError(w, "invalid request: "+err.Error())
			return
		}

		res := planner.Plan(intervals, frozen)

		if res.Status == "NO_SAMPLE_POINT" {
			writeJSON(w, http.StatusOK, responseFail{
				Status:          res.Status,
				SampleCount:     0,
				Points:          []pointJSON{},
				ProcessingOrder: res.Order,
				FailedInterval:  res.Fail.IntervalID,
				FailedPosition:  res.Fail.Position,
				Missing:         res.Fail.Missing,
			})
			return
		}

		points := make([]pointJSON, 0, len(res.Points))
		for _, p := range res.Points {
			points = append(points, pointJSON{Batch: p.Batch, CoveredIDs: p.Covered})
		}
		hits := make([]intervalHitsJSON, 0, len(res.Hits))
		for _, h := range res.Hits {
			hits = append(hits, intervalHitsJSON{ID: h.ID, Batches: h.Batches})
		}
		writeJSON(w, http.StatusOK, responseOK{
			Status:          "OK",
			SampleCount:     len(points),
			Points:          points,
			IntervalHits:    hits,
			ProcessingOrder: res.Order,
		})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return mux
}
