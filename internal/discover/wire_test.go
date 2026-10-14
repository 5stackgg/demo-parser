package discover

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
)

func TestCandidateWithAnUnsettledSpreadStillEncodes(t *testing.T) {
	body, err := json.Marshal(Response{Candidates: []Candidate{{
		Stand:  r3.Vector{X: 1, Y: 2, Z: 3},
		Spread: math.Inf(1),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"spread":null`) {
		t.Fatalf("spread not null: %s", body)
	}
	if !strings.Contains(string(body), `"stand":{"x":1,"y":2,"z":3}`) {
		t.Fatalf("stand not a point: %s", body)
	}
}
