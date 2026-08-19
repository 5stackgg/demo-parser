package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/5stackgg/demo-parser/internal/simulate"
)

// meshRevisions serves the local replay-map-meshes clone n times over, each on
// its own base URL. Two of them are two mesh revisions as far as the loader is
// concerned, built independently from identical bytes — which is how the
// endpoint gets exercised end to end without inventing a fake .tri.
func meshRevisions(t *testing.T, n int) []string {
	t.Helper()
	dir := meshFixtureDir(t)
	if dir == "" {
		t.Skip("no replay-map-meshes clone found above the working directory; set MAP_MESH_FIXTURES")
	}
	// Both revisions have to stay resident for the whole request or every
	// lineup pays for a refetch.
	t.Setenv("MAP_MESH_CACHE", "4")
	refs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
		t.Cleanup(srv.Close)
		refs = append(refs, srv.URL)
	}
	return refs
}

func mirageLineups(n int) []simulate.LineupSeed {
	out := make([]simulate.LineupSeed, 0, n)
	types := []string{"Smoke", "HE", "Flash", "Molotov"}
	for i := 0; i < n; i++ {
		out = append(out, simulate.LineupSeed{
			ID:              string(rune('a'+i%26)) + "-lineup",
			NadeType:        types[i%len(types)],
			InitialPosition: &simulate.Point{X: -2300, Y: 0, Z: -64},
			InitialVelocity: &simulate.Point{X: 500, Y: float64(i%40)*10 - 200, Z: 200},
		})
	}
	return out
}

// The end-to-end shape of the contract the API codes against, and the property
// it rests on: the same mesh on both sides reports no drift at all.
func TestDriftEndpointOnARealMap(t *testing.T) {
	refs := meshRevisions(t, 2)
	w := post(t, handleDrift, simulate.DriftRequest{
		Map:     "de_mirage",
		From:    refs[0],
		To:      refs[1],
		Lineups: mirageLineups(16),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	// Decoded as a bare map first: these key names are the contract.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"map", "from", "to", "constants", "thresholds", "summary", "results", "caveats"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response is missing %q: %v", key, raw)
		}
	}
	results, _ := raw["results"].([]any)
	if len(results) != 16 {
		t.Fatalf("got %d results, want 16", len(results))
	}
	first, _ := results[0].(map[string]any)
	for _, key := range []string{"index", "id", "verdict", "from", "to"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("result is missing %q: %v", key, first)
		}
	}
	// The one field name that has to survive every refactor: it is the whole
	// warning label on this endpoint.
	side, _ := first["to"].(map[string]any)
	if _, ok := side["comparison_point"]; !ok {
		t.Fatalf("an outcome must report a comparison_point, not a landing: %v", side)
	}

	var res simulate.DriftResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Summary.Moved != 0 || res.Summary.Broken != 0 {
		t.Fatalf("identical geometry must not report drift: %+v", res.Summary)
	}
	if res.Summary.Unchanged == 0 {
		t.Fatal("nothing resolved; the test is not exercising the simulator")
	}
	if len(res.Caveats) == 0 {
		t.Fatal("the response must carry its caveats")
	}
	if res.From == "" || res.To == "" {
		t.Fatal("the response must echo the revisions it compared")
	}
}

// An empty revision means "whatever this process is pinned to", and the
// response says which that was rather than echoing the blank.
func TestDriftEndpointResolvesTheDefaultRevision(t *testing.T) {
	refs := meshRevisions(t, 1)
	t.Setenv("MAP_MESH_CDN", refs[0])
	w := post(t, handleDrift, simulate.DriftRequest{
		Map:     "de_mirage",
		Lineups: mirageLineups(2),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var res simulate.DriftResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.From != refs[0] || res.To != refs[0] {
		t.Fatalf("an empty revision should resolve to the pinned base, got from=%q to=%q", res.From, res.To)
	}
}

func TestDriftEndpointStreamsNDJSON(t *testing.T) {
	refs := meshRevisions(t, 2)
	w := post(t, handleDrift, simulate.DriftRequest{
		Map:     "de_mirage",
		From:    refs[0],
		To:      refs[1],
		Lineups: mirageLineups(12),
		Stream:  true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}

	var (
		kinds   []string
		results int
		summary simulate.DriftSummary
	)
	scan := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for scan.Scan() {
		line := scan.Bytes()
		var envelope struct {
			Type    string                `json:"type"`
			Index   int                   `json:"index"`
			Verdict string                `json:"verdict"`
			Summary simulate.DriftSummary `json:"summary"`
			Error   string                `json:"error"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			t.Fatalf("line is not JSON: %s", line)
		}
		kinds = append(kinds, envelope.Type)
		switch envelope.Type {
		case "result":
			if envelope.Index != results {
				t.Fatalf("result %d arrived out of order (index %d)", results, envelope.Index)
			}
			if envelope.Verdict == "" {
				t.Fatalf("result %d has no verdict: %s", results, line)
			}
			results++
		case "summary":
			summary = envelope.Summary
		case "error":
			t.Fatalf("stream reported an error: %s", envelope.Error)
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kinds[0] != "header" || kinds[len(kinds)-1] != "summary" {
		t.Fatalf("stream should open with a header and close with a summary, got %v", kinds)
	}
	if results != 12 {
		t.Fatalf("streamed %d results, want 12", results)
	}
	if summary.Lineups != 12 || summary.Moved != 0 || summary.Broken != 0 {
		t.Fatalf("identical geometry must not report drift: %+v", summary)
	}
}

// A batch too big for one JSON body is refused with the fix in the message,
// rather than being answered with a response nothing can hold.
func TestDriftEndpointRefusesAnUnbufferableBatch(t *testing.T) {
	// No mesh is fetched: the size check runs before anything is loaded, which
	// is the point of putting it first.
	w := post(t, handleDrift, simulate.DriftRequest{
		Map:     "de_mirage",
		Lineups: make([]simulate.LineupSeed, simulate.MaxBufferedLineups+1),
	})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stream") {
		t.Fatalf("the error should point at streaming: %s", w.Body.String())
	}

	w = post(t, handleDrift, simulate.DriftRequest{
		Map:     "de_mirage",
		Stream:  true,
		Lineups: make([]simulate.LineupSeed, simulate.MaxLineups+1),
	})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the hard cap: status %d: %s", w.Code, w.Body.String())
	}
}

func TestDriftEndpointRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name string
		req  simulate.DriftRequest
		want int
	}{
		{"no map", simulate.DriftRequest{Lineups: mirageLineups(1)}, http.StatusBadRequest},
		{"no lineups", simulate.DriftRequest{Map: "de_mirage"}, http.StatusBadRequest},
		{
			"revision that walks out of the pinned path",
			simulate.DriftRequest{Map: "de_mirage", From: "../../../etc", Lineups: mirageLineups(1)},
			http.StatusBadRequest,
		},
		{
			"revision with a slash in the tag",
			simulate.DriftRequest{Map: "de_mirage", To: "repo@tag/../..", Lineups: mirageLineups(1)},
			http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := post(t, handleDrift, tc.req)
			if w.Code != tc.want {
				t.Fatalf("status %d (want %d): %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// A map with no published .tri at one of the two revisions is a 404 that says
// WHICH side is missing: a bad old tag and an unpublished new mesh set are
// different problems with different fixes.
func TestDriftEndpointNamesTheMissingSide(t *testing.T) {
	refs := meshRevisions(t, 2)
	w := post(t, handleDrift, simulate.DriftRequest{
		Map:     "de_nonexistent",
		From:    refs[0],
		To:      refs[1],
		Lineups: mirageLineups(1),
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Body.String(), "from:") {
		t.Fatalf("the error should name the side that is missing: %s", w.Body.String())
	}
}

func TestDriftEndpointRejectsNonPost(t *testing.T) {
	w := httptest.NewRecorder()
	handleDrift(w, httptest.NewRequest(http.MethodGet, "/drift", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", w.Code)
	}
}
