package parser

import (
	"errors"
	"fmt"
	"math"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// One-way detection.
//
// The thing to understand first: an eye-to-eye sightline cannot be one-way. The
// optical depth along a segment is the same integral in both directions, and
// the map occludes a segment or it does not, so "A's eyes see B's eyes" and "B's
// eyes see A's eyes" are the same question asked twice.
//
// A real one-way is therefore never about the line between two eyes. It comes
// from the two sides not being the same shape:
//
//   - Stance. Crouching drops an eye 18 units. If the cloud has a gap under it
//     or a lip over it, that is the difference between looking through smoke and
//     looking under it — and the same drop applied to the other player moves
//     them out of view rather than into it.
//   - Body extent. Seeing someone means seeing any part of them, and the ray to
//     their knees is not the ray from their knees to you. A player whose head
//     alone clears the cloud is visible while seeing nothing.
//
// So the test is run over both stances on both sides, in both directions, with
// the target treated as a body rather than a point. What comes back is honest
// about resting on assumed eye heights and on a density model that knows
// nothing about how CS2 lights a cloud.

// CS2 view offsets above the player's feet. These are the engine's long-
// standing values (the same 64/46 split CS:GO shipped), not something measured
// off a demo here — a demo would give them exactly, as PositionEyes() minus
// Position(), and calibrating against one is the obvious next step. An error
// here shifts an eye by at most a unit or two, which matters only for a line
// already grazing the edge of a cloud — which is exactly the case one-ways live
// in, so treat marginal calls as marginal.
const (
	standEyeHeight  = 64.09
	crouchEyeHeight = 46.08
)

const (
	stanceStand  = "stand"
	stanceCrouch = "crouch"
)

// OneWayRequest tests pairs of player positions for asymmetric visibility.
type OneWayRequest struct {
	Map    string      `json:"map"`
	Smokes []CloudSpec `json:"smokes,omitempty"`
	Smoke  *CloudSpec  `json:"smoke,omitempty"`
	At     *Point      `json:"at,omitempty"`
	// Pairs are two players' positions. From is side A, To is side B.
	Pairs []SightlinePair `json:"pairs"`
	// Positions says what the pair's Z means: "feet" (default, a standing
	// position as the game reports it) or "eyes", in which case the standing
	// eye height is subtracted to recover the feet. Getting this wrong moves
	// every eye by 64 units, so it is explicit rather than guessed.
	Positions string   `json:"positions,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	// Eye-height overrides, for callers who have measured better numbers than
	// the constants above.
	StandEyeHeight  *float64 `json:"stand_eye_height,omitempty"`
	CrouchEyeHeight *float64 `json:"crouch_eye_height,omitempty"`
}

func (r OneWayRequest) sightlineRequest() SightlineRequest {
	return SightlineRequest{Smokes: r.Smokes, Smoke: r.Smoke, At: r.At}
}

func (r OneWayRequest) threshold() float64 {
	if r.Threshold == nil || *r.Threshold <= 0 {
		return DefaultBlockThreshold
	}
	return *r.Threshold
}

func (r OneWayRequest) eyeHeights() (stand, crouch float64) {
	stand, crouch = standEyeHeight, crouchEyeHeight
	if r.StandEyeHeight != nil && *r.StandEyeHeight > 0 {
		stand = *r.StandEyeHeight
	}
	if r.CrouchEyeHeight != nil && *r.CrouchEyeHeight > 0 {
		crouch = *r.CrouchEyeHeight
	}
	return stand, crouch
}

// OneWayView is what one player can see of the other in one stance pairing.
type OneWayView struct {
	Visible bool `json:"visible"`
	// Depth is the smoke on the clearest line to any part of the target, in
	// cell widths of full density; Transmittance is e^-Depth. When every part
	// of the target is behind the map, Depth describes the eye-to-eye line and
	// WorldBlocked says the map decided it.
	Depth         float64 `json:"depth"`
	Transmittance float64 `json:"transmittance"`
	WorldBlocked  bool    `json:"world_blocked"`
	// SamplesVisible of Samples body points were both clear of the map and
	// under the threshold. One visible sample out of seven is a sliver of a
	// shoulder, which is why this is reported rather than folded into Visible.
	SamplesVisible int `json:"samples_visible"`
	Samples        int `json:"samples"`
}

// OneWayStance is one (A stance, B stance) pairing, tested both ways.
type OneWayStance struct {
	AStance string     `json:"a_stance"`
	BStance string     `json:"b_stance"`
	AToB    OneWayView `json:"a_to_b"`
	BToA    OneWayView `json:"b_to_a"`
	OneWay  bool       `json:"one_way"`
	// Favors names the side that can see while the other cannot.
	Favors string `json:"favors,omitempty"`
	// Cause is "smoke" when the blind side is stopped by the cloud, "world"
	// when the map alone does it (a ledge, not a lineup), and empty when the
	// pairing is symmetric.
	Cause string `json:"cause,omitempty"`
	// Margin is how much room the call has: the smaller of how far the seeing
	// side sits under the threshold and how far the blind side sits over it.
	// Zero or less means the answer would flip on a small change of threshold,
	// and a world-caused verdict reports zero because a wall is not a matter of
	// degree.
	Margin float64 `json:"margin"`
}

// OneWayResult is one pair's verdict across every stance pairing.
type OneWayResult struct {
	OneWay bool   `json:"one_way"`
	Favors string `json:"favors,omitempty"`
	Cause  string `json:"cause,omitempty"`
	// Confidence is "none" when nothing is asymmetric, then "marginal",
	// "likely" or "strong" as the margin grows. It grades the geometry only —
	// see Caveats.
	Confidence string `json:"confidence"`
	// Contested marks the case where different stance pairings favour
	// different sides — typically "whoever crouches wins". The advantage then
	// belongs to whoever picks the right stance rather than to a position, so
	// Favors alone would be misleading.
	Contested bool `json:"contested,omitempty"`
	// Best is the stance pairing with the widest margin, i.e. the one to
	// actually stand in. Nil when no pairing is one-way.
	Best    *OneWayStance  `json:"best,omitempty"`
	Stances []OneWayStance `json:"stances"`
}

type OneWayResponse struct {
	Map       string         `json:"map"`
	Threshold float64        `json:"threshold"`
	Smokes    []CloudInfo    `json:"smokes"`
	Results   []OneWayResult `json:"results"`
	// Caveats is returned on every response, not only on marginal ones. This
	// is a geometric model and the thing it is modelling is partly a renderer.
	Caveats []string `json:"caveats"`
}

// oneWayCaveats are the limits of this model, stated on every response so a UI
// can put them in front of whoever is about to trust one.
var oneWayCaveats = []string{
	"eye heights are the engine's standing/crouching view offsets, not measured per map or per stance transition",
	"CS2 lights smoke volumetrically and a target's contrast against its background is not modelled, so a cloud can be one-way in game while this reports it symmetric",
	"eye-to-eye is symmetric by construction: every asymmetry here comes from stance or from which parts of a body each side can see",
	"a marginal verdict rests on the density threshold rather than on the geometry, and should be treated as unverified",
}

// Margins, in optical depth, for grading a verdict. One unit is a factor of e
// in how much of the target survives, so a full unit of clearance on both sides
// means the call does not hinge on the exact threshold. These are chosen, not
// measured.
const (
	strongMargin = 1.5
	likelyMargin = 0.5
)

// OneWay tests each pair for asymmetric visibility through the supplied clouds.
func OneWay(mesh *geometry.Mesh, req OneWayRequest) (OneWayResponse, error) {
	if mesh == nil {
		return OneWayResponse{}, ErrNoMesh
	}
	if len(req.Pairs) == 0 {
		return OneWayResponse{}, errors.New("pairs must not be empty")
	}
	if len(req.Pairs) > maxSightlinePairs {
		return OneWayResponse{}, fmt.Errorf("too many pairs: %d (max %d)", len(req.Pairs), maxSightlinePairs)
	}
	switch req.Positions {
	case "", "feet", "eyes":
	default:
		return OneWayResponse{}, fmt.Errorf("positions must be \"feet\" or \"eyes\", got %q", req.Positions)
	}
	clouds, infos, err := resolveClouds(mesh, req.sightlineRequest().clouds())
	if err != nil {
		return OneWayResponse{}, err
	}

	stand, crouch := req.eyeHeights()
	threshold := req.threshold()
	out := OneWayResponse{
		Map:       req.Map,
		Threshold: threshold,
		Smokes:    infos,
		Caveats:   oneWayCaveats,
	}
	for i, pair := range req.Pairs {
		if !pair.From.valid() || !pair.To.valid() {
			return OneWayResponse{}, fmt.Errorf("pair %d: coordinates must be finite", i)
		}
		a, b := pair.From.vec(), pair.To.vec()
		if req.Positions == "eyes" {
			a.Z -= stand
			b.Z -= stand
		}
		out.Results = append(out.Results, oneWayPair(mesh, clouds, a, b, stand, crouch, threshold))
	}
	return out, nil
}

func oneWayPair(mesh *geometry.Mesh, clouds []resolvedCloud, aFeet, bFeet r3.Vector, stand, crouch, threshold float64) OneWayResult {
	height := map[string]float64{stanceStand: stand, stanceCrouch: crouch}
	res := OneWayResult{Confidence: "none"}
	for _, as := range [2]string{stanceStand, stanceCrouch} {
		for _, bs := range [2]string{stanceStand, stanceCrouch} {
			aEye := raise(aFeet, height[as])
			bEye := raise(bFeet, height[bs])
			st := OneWayStance{
				AStance: as,
				BStance: bs,
				AToB:    look(mesh, clouds, aEye, bEye, bFeet, threshold),
				BToA:    look(mesh, clouds, bEye, aEye, aFeet, threshold),
			}
			if st.AToB.Visible != st.BToA.Visible {
				seeing, blind := st.AToB, st.BToA
				st.OneWay, st.Favors = true, "a"
				if st.BToA.Visible {
					seeing, blind = st.BToA, st.AToB
					st.Favors = "b"
				}
				if blind.WorldBlocked {
					// The blind side is behind the map, so its depth says
					// nothing about how safe the call is and there is no
					// threshold for the verdict to be sensitive to.
					st.Cause, st.Margin = "world", 0
				} else {
					st.Cause = "smoke"
					st.Margin = math.Min(threshold-seeing.Depth, blind.Depth-threshold)
				}
			}
			res.Stances = append(res.Stances, st)
		}
	}

	best := -1
	favored := map[string]bool{}
	for i, st := range res.Stances {
		if !st.OneWay {
			continue
		}
		favored[st.Favors] = true
		if best < 0 || better(st, res.Stances[best]) {
			best = i
		}
	}
	if best < 0 {
		return res
	}
	st := res.Stances[best]
	res.OneWay, res.Favors, res.Cause = true, st.Favors, st.Cause
	res.Contested = len(favored) > 1
	res.Best = &res.Stances[best]
	res.Confidence = grade(st)
	return res
}

// better ranks one one-way pairing above another: the wider margin wins, and
// where there is no margin to compare — a world-caused verdict, or two pairings
// equally clear of the threshold — the one exposing more of the target does.
func better(a, b OneWayStance) bool {
	if a.Margin != b.Margin {
		return a.Margin > b.Margin
	}
	return seeingView(a).SamplesVisible > seeingView(b).SamplesVisible
}

func seeingView(st OneWayStance) OneWayView {
	if st.Favors == "b" {
		return st.BToA
	}
	return st.AToB
}

// grade turns a margin into a word. Sliver visibility is capped at "marginal"
// however wide the margin looks: when a single body sample carries the seeing
// side, the verdict is really a statement about where this model put that
// sample, not about the smoke.
func grade(st OneWayStance) string {
	seeing := seeingView(st)
	switch {
	case seeing.SamplesVisible <= 1:
		return "marginal"
	case st.Cause == "world":
		// The map either occludes or it does not. There is no threshold for
		// the verdict to be sensitive to, only the fidelity of the mesh.
		return "strong"
	case st.Margin >= strongMargin:
		return "strong"
	case st.Margin >= likelyMargin:
		return "likely"
	default:
		return "marginal"
	}
}

func raise(feet r3.Vector, h float64) r3.Vector {
	return r3.Vector{X: feet.X, Y: feet.Y, Z: feet.Z + h}
}

// look reports what an observer at eye can see of a player with eyes at
// targetEye standing on targetFeet: the clearest line to any part of them.
func look(mesh *geometry.Mesh, clouds []resolvedCloud, eye, targetEye, targetFeet r3.Vector, threshold float64) OneWayView {
	pts := append([]r3.Vector{targetEye}, bodySamplePoints(eye, targetEye, targetFeet)...)
	view := OneWayView{Samples: len(pts), Depth: math.Inf(1), WorldBlocked: true}
	for _, p := range pts {
		if mesh.Occluded(eye, p) {
			continue
		}
		view.WorldBlocked = false
		depth := 0.0
		for _, c := range clouds {
			depth += c.depth(eye, p)
		}
		if depth < view.Depth {
			view.Depth = depth
		}
		if depth < threshold {
			view.SamplesVisible++
		}
	}
	if view.WorldBlocked {
		// Nothing to report a depth for; describe the eye-to-eye line so the
		// number still means something to a caller comparing directions.
		view.Depth = 0
		for _, c := range clouds {
			view.Depth += c.depth(eye, targetEye)
		}
	}
	view.Visible = view.SamplesVisible > 0
	view.Transmittance = math.Exp(-view.Depth)
	return view
}
