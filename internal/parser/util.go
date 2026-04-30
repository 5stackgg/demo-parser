package parser

import (
	"math"
	"regexp"
	"strconv"

	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// CS2 demos record workshop maps as `workshop/<numeric-id>/<map-name>`
// (e.g. `workshop/3070821578/de_torn`). Stock maps record as plain names.
// The id lets a streamer pre-download the .vpk via steamcmd.
var workshopMapRe = regexp.MustCompile(`^workshop/(\d+)/`)

// CS2 weapon max horizontal speeds (units/second). Used for the
// counter-strafing 34%-of-maxspeed threshold (where the engine's
// move-while-shooting inaccuracy kicks in). Rifles only.
var weaponMaxSpeed = map[common.EquipmentType]float32{
	common.EqAK47:   215,
	common.EqM4A4:   225,
	common.EqM4A1:   225,
	common.EqFamas:  220,
	common.EqGalil:  215,
	common.EqAUG:    220,
	common.EqSG553:  210,
	common.EqSSG08:  230,
	common.EqAWP:    200,
	common.EqScar20: 215,
	common.EqG3SG1:  215,
}

func teamCode(t common.Team) string {
	switch t {
	case common.TeamCounterTerrorists:
		return "ct"
	case common.TeamTerrorists:
		return "t"
	default:
		return ""
	}
}

func steamIDStr(p *common.Player) string {
	if p == nil || p.SteamID64 == 0 {
		return ""
	}
	return strconv.FormatUint(p.SteamID64, 10)
}

func bombSiteCode(s events.Bombsite) string {
	switch s {
	case events.BombsiteA:
		return "A"
	case events.BombsiteB:
		return "B"
	default:
		return ""
	}
}

func grenadeTypeCode(t common.EquipmentType) string {
	switch t {
	case common.EqFlash:
		return "Flash"
	case common.EqHE:
		return "HE"
	case common.EqSmoke:
		return "Smoke"
	case common.EqMolotov, common.EqIncendiary:
		return "Molotov"
	case common.EqDecoy:
		return "Decoy"
	default:
		return ""
	}
}

func angleBetweenDeg(a, b r3.Vector) float32 {
	la := math.Sqrt(a.X*a.X + a.Y*a.Y + a.Z*a.Z)
	lb := math.Sqrt(b.X*b.X + b.Y*b.Y + b.Z*b.Z)
	if la == 0 || lb == 0 {
		return 180
	}
	cos := (a.X*b.X + a.Y*b.Y + a.Z*b.Z) / (la * lb)
	if cos > 1 {
		cos = 1
	} else if cos < -1 {
		cos = -1
	}
	return float32(math.Acos(cos) * 180 / math.Pi)
}

// viewVector converts CS eye angles to a unit vector. Z-up; pitch > 0 looks down.
func viewVector(yawDeg, pitchDeg float32) r3.Vector {
	yaw := float64(yawDeg) * math.Pi / 180
	pitch := float64(pitchDeg) * math.Pi / 180
	cp := math.Cos(pitch)
	return r3.Vector{
		X: cp * math.Cos(yaw),
		Y: cp * math.Sin(yaw),
		Z: -math.Sin(pitch),
	}
}
