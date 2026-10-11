package discover

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sort"
	"testing"

	"github.com/golang/geo/r3"
)

// testdata/de_mirage_throws.json is every seeded throw the practice plugin
// recorded on de_mirage: the seed the engine wrote, the path it flew and where
// it went off. It is the ground truth the model is fitted to.
type recordedThrow struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Seed struct {
		Position r3.Vector `json:"position"`
		Velocity r3.Vector `json:"velocity"`
	} `json:"seed"`
	Landing  r3.Vector `json:"landing"`
	FlightMs int       `json:"flight_ms"`
	Points   []struct {
		T float64 `json:"t"`
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	} `json:"points"`
}

func recordedThrows(t *testing.T) []recordedThrow {
	t.Helper()
	raw, err := os.ReadFile("testdata/de_mirage_throws.json")
	if err != nil {
		t.Fatal(err)
	}
	var throws []recordedThrow
	if err := json.Unmarshal(raw, &throws); err != nil {
		t.Fatal(err)
	}
	return throws
}

// Every recorded arc is the parabola under 320 u/s² with no drag, to a
// hundredth: the flight up to its first impact is exact.
func TestRecordedArcsFallAtTheModelsGravity(t *testing.T) {
	for _, th := range recordedThrows(t) {
		p0, v0 := th.Seed.Position, th.Seed.Velocity
		axis := func(p r3.Vector) float64 { return p.X }
		speed := v0.X
		if math.Abs(v0.Y) > math.Abs(v0.X) {
			axis, speed = func(p r3.Vector) float64 { return p.Y }, v0.Y
		}
		for _, p := range th.Points {
			at := r3.Vector{X: p.X, Y: p.Y, Z: p.Z}
			dt := (axis(at) - axis(p0)) / speed
			if dt < 0.3 {
				continue
			}
			g := 2 * (p0.Z + v0.Z*dt - at.Z) / (dt * dt)
			if math.Abs(g-DefaultModel().Gravity) > 0.1 {
				t.Fatalf("%s falls at %.3f u/s², not %.0f", th.Name, g, DefaultModel().Gravity)
			}
			break
		}
	}
}

// Where the model puts each recorded smoke against where the engine did.
// Half of them land within a couple of units. The rest go wrong at one bounce
// off a softer surface (a roof, an awning) that keeps less speed than the
// mesh can know about; that error is why a candidate is only ever a proposal.
func TestModelLandsWhereTheEngineDid(t *testing.T) {
	world := mirageWorld(t)
	m := DefaultModel()
	var misses []float64
	exact := 0
	for _, th := range recordedThrows(t) {
		if th.Type != "Smoke" {
			continue
		}
		f := m.Fly(world, th.Seed.Position, th.Seed.Velocity, false)
		miss := f.Rest.Sub(th.Landing).Norm()
		t.Logf("%-34s %6.1f off after %d bounces", th.Name, miss, f.Bounces)
		misses = append(misses, miss)
		if miss <= 5 {
			exact++
		}
	}
	sort.Float64s(misses)
	if exact < 5 {
		t.Fatalf("%d of %d smokes within 5 units, want at least 5", exact, len(misses))
	}
	if median := misses[len(misses)/2]; median > 35 {
		t.Fatalf("median miss %.1f, want at most 35", median)
	}
}

// From the stance Jungle Smoke from Palace Alley was recorded at, a jump throw
// search finds the aim it was recorded with.
func TestSearchRediscoversARecordedLineup(t *testing.T) {
	world := mirageWorld(t)
	stance := v(908.68005, -1036.0312, -251.96875)
	req := Request{
		Map:        "de_mirage",
		Target:     v(-637.4771, -1677.9963, -173.96875),
		Radius:     40,
		Area:       Area{Min: stance.Sub(v(0, 0, 16)), Max: stance.Add(v(0, 0, 16))},
		Techniques: []Technique{{JumpThrow: true}},
		Strengths:  []Strength{Full},
		BudgetMs:   20000,
	}
	res, err := Search(context.Background(), world, req, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Candidates {
		if math.Abs(c.Pitch-(-55.324947)) < 0.1 && math.Abs(angleDelta(c.Yaw, -161.02596)) < 0.1 {
			return
		}
	}
	t.Fatalf("the recorded aim (-55.32, -161.03) is not among %+v", res.Candidates)
}
