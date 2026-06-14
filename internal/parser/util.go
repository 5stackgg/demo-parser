package parser

import (
	"math"
	"regexp"
	"strconv"
	"strings"

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

// weaponName maps a CS2 equipment type to its canonical internal weapon
// name — the engine classname without the `weapon_` prefix (e.g. EqM4A4 ->
// "m4a1", EqM4A1 -> "m4a1_silencer", EqP2000 -> "hkp2000"). demoinfocs'
// String() returns display names ("M4A4", "M4A1-S", "Desert Eagle") that
// don't line up with the names native 5Stack matches store or the equipment
// icon set, which is what made imported matches show duplicate / missing
// weapons. Emitting the canonical name keeps both paths consistent.
func weaponName(t common.EquipmentType) string {
	switch t {
	// pistols
	case common.EqP2000:
		return "hkp2000"
	case common.EqGlock:
		return "glock"
	case common.EqP250:
		return "p250"
	case common.EqDeagle:
		return "deagle"
	case common.EqFiveSeven:
		return "fiveseven"
	case common.EqDualBerettas:
		return "elite"
	case common.EqTec9:
		return "tec9"
	case common.EqCZ:
		return "cz75a"
	case common.EqUSP:
		return "usp_silencer"
	case common.EqRevolver:
		return "revolver"
	// smgs
	case common.EqMP7:
		return "mp7"
	case common.EqMP9:
		return "mp9"
	case common.EqBizon:
		return "bizon"
	case common.EqMac10:
		return "mac10"
	case common.EqUMP:
		return "ump45"
	case common.EqP90:
		return "p90"
	case common.EqMP5:
		return "mp5sd"
	// heavy
	case common.EqSawedOff:
		return "sawedoff"
	case common.EqNova:
		return "nova"
	case common.EqMag7:
		return "mag7"
	case common.EqXM1014:
		return "xm1014"
	case common.EqM249:
		return "m249"
	case common.EqNegev:
		return "negev"
	// rifles
	case common.EqGalil:
		return "galilar"
	case common.EqFamas:
		return "famas"
	case common.EqAK47:
		return "ak47"
	case common.EqM4A4:
		return "m4a1"
	case common.EqM4A1:
		return "m4a1_silencer"
	case common.EqSSG08:
		return "ssg08"
	case common.EqSG556:
		return "sg556"
	case common.EqAUG:
		return "aug"
	case common.EqAWP:
		return "awp"
	case common.EqScar20:
		return "scar20"
	case common.EqG3SG1:
		return "g3sg1"
	// equipment / utility
	case common.EqZeus:
		return "taser"
	case common.EqBomb:
		return "c4"
	case common.EqKnife:
		return "knife"
	case common.EqDecoy:
		return "decoy"
	case common.EqMolotov:
		return "molotov"
	case common.EqIncendiary:
		return "inferno"
	case common.EqFlash:
		return "flashbang"
	case common.EqSmoke:
		return "smokegrenade"
	case common.EqHE:
		return "hegrenade"
	default:
		return ""
	}
}

// weaponCanonical resolves an equipment instance to its canonical name,
// falling back to a sanitised display name for anything outside the known
// set so an unusual item is still stored as a stable lowercase token rather
// than dropped.
func weaponCanonical(e *common.Equipment) string {
	if e == nil {
		return ""
	}
	if name := weaponName(e.Type); name != "" {
		return name
	}
	return strings.ToLower(strings.ReplaceAll(e.String(), " ", ""))
}

// activeWeaponName returns the canonical name of the player's currently
// equipped weapon (rifle, pistol, knife, grenade, …), or "" if unarmed.
// Same naming as kills/shots so the web icon map resolves it directly.
func activeWeaponName(p *common.Player) string {
	if p == nil {
		return ""
	}
	return weaponCanonical(p.ActiveWeapon())
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

func grenadeValue(p *common.Player) int {
	if p == nil {
		return 0
	}
	total := 0
	for _, w := range p.Weapons() {
		if w == nil {
			continue
		}
		switch w.Type {
		case common.EqFlash:
			total += 200
		case common.EqSmoke:
			total += 300
		case common.EqHE:
			total += 300
		case common.EqMolotov, common.EqIncendiary:
			total += 400
		case common.EqDecoy:
			total += 50
		}
	}
	return total
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

// f32ptr returns a pointer to the float32 form of v. Used for optional
// coordinate fields so they serialize only when actually captured.
func f32ptr(v float64) *float32 {
	f := float32(v)
	return &f
}
