package parser

import "strconv"

// PlaybackSchemaVersion is the playback blob shape BuildPlayback produces: the
// api's DEMO_METADATA_VERSION. The api names the version it wants on each
// request and builds the blob itself when this one differs, so the two can be
// deployed in either order.
const PlaybackSchemaVersion = 11

// PlaybackBlob is the 2D/3D replay's document: what the api's
// buildPlaybackBlob (api/src/demos/demo-metadata.service.ts) made from this
// parser's JSON, built here so the parse no longer has to cross the wire as
// 50 MB of JSON only to be reshaped and compressed on the api's event loop.
//
// It has to come out the same as the api's, field for field. That function
// read this parser's JSON, where a zero value tagged omitempty is absent, and
// filled the gaps with `?? null`, `?? 0` or `?? []` — so each field below says
// which it was, and a nullable one is a pointer that is nil exactly when the
// parser would have left the key out.
type PlaybackBlob struct {
	SchemaVersion       int                 `json:"schema_version"`
	ParserSchemaVersion int                 `json:"parser_schema_version"`
	MatchMapID          string              `json:"match_map_id"`
	TickRate            float64             `json:"tick_rate"`
	TotalTicks          int                 `json:"total_ticks"`
	MapName             string              `json:"map_name"`
	RoundTicks          []RoundTick         `json:"round_ticks"`
	Players             []PlayerInfo        `json:"players"`
	Kills               []EventKill         `json:"kills"`
	Bombs               []EventBomb         `json:"bombs"`
	KitDrops            []EventKitDrop      `json:"kit_drops"`
	Positions           []playbackPosition  `json:"positions"`
	ShotsFired          []playbackShot      `json:"shots_fired"`
	GrenadeThrows       []playbackGrenade   `json:"grenade_throws"`
	GrenadeTrajectories []GrenadeTrajectory `json:"grenade_trajectories"`
	SmokeVolumes        []EventSmokeVolume  `json:"smoke_volumes"`
	SmokeVoxels         []EventSmokeVoxels  `json:"smoke_voxels"`
	Infernos            []EventInferno      `json:"infernos"`
	Damages             []playbackDamage    `json:"damages"`
	Flashes             []playbackFlash     `json:"flashes"`
	RoundInventory      []playbackInventory `json:"round_inventory"`
}

type playbackPosition struct {
	Round        int      `json:"round"`
	Tick         int      `json:"tick"`
	Attacker     *string  `json:"attacker_steam_id"`
	Team         *string  `json:"attacker_team"`
	Alive        bool     `json:"alive"`
	X            float32  `json:"x"`
	Y            float32  `json:"y"`
	Z            float32  `json:"z"`
	Yaw          *float32 `json:"yaw"`
	Pitch        *float32 `json:"pitch"`
	Health       *int     `json:"health"`
	Armor        *int     `json:"armor"`
	Helmet       bool     `json:"helmet"`
	HasBomb      bool     `json:"has_bomb"`
	HasDefuser   bool     `json:"has_defuser"`
	ActiveWeapon *string  `json:"active_weapon"`
	Ducked       bool     `json:"ducked"`
}

type playbackShot struct {
	Round    int      `json:"round"`
	Tick     int      `json:"tick"`
	Attacker *string  `json:"attacker_steam_id"`
	Team     *string  `json:"attacker_team"`
	With     *string  `json:"with"`
	Yaw      *float32 `json:"yaw"`
	Pitch    *float32 `json:"pitch"`
	EyeX     *float32 `json:"eye_x"`
	EyeY     *float32 `json:"eye_y"`
	EyeZ     *float32 `json:"eye_z"`
	Result   *string  `json:"result"`
	ImpactX  *float32 `json:"impact_x"`
	ImpactY  *float32 `json:"impact_y"`
	ImpactZ  *float32 `json:"impact_z"`
	MissX    *float32 `json:"miss_x"`
	MissY    *float32 `json:"miss_y"`
	MissZ    *float32 `json:"miss_z"`
}

type playbackGrenade struct {
	Round     int     `json:"round"`
	Tick      int     `json:"tick"`
	GrenadeID *int    `json:"grenade_id"`
	Thrower   *string `json:"thrower_steam_id"`
	Team      *string `json:"thrower_team"`
	Type      string  `json:"type"`
	Phase     string  `json:"phase"`
	X         float32 `json:"x"`
	Y         float32 `json:"y"`
	Z         float32 `json:"z"`
}

type playbackDamage struct {
	Round       int     `json:"round"`
	Time        string  `json:"time"`
	Attacker    *string `json:"attacker_steam_id"`
	Victim      *string `json:"attacked_steam_id"`
	Weapon      *string `json:"weapon"`
	Hitgroup    int     `json:"hitgroup"`
	Damage      int     `json:"damage"`
	DamageArmor int     `json:"damage_armor"`
	Health      *int    `json:"health"`
}

type playbackFlash struct {
	Round     int     `json:"round"`
	Tick      int     `json:"tick"`
	Attacker  *string `json:"attacker_steam_id"`
	Victim    *string `json:"attacked_steam_id"`
	Duration  float64 `json:"duration"`
	TeamFlash bool    `json:"team_flash"`
}

type playbackInventory struct {
	Round     int     `json:"round"`
	SteamID   *string `json:"steam_id"`
	Team      *string `json:"team"`
	Flash     int     `json:"flash"`
	Smoke     int     `json:"smoke"`
	HE        int     `json:"he"`
	Molotov   int     `json:"molotov"`
	Decoy     int     `json:"decoy"`
	Primary   *string `json:"primary"`
	Secondary *string `json:"secondary"`
	Armor     int     `json:"armor"`
	Helmet    bool    `json:"helmet"`
	Kit       bool    `json:"kit"`
}

// Each of these is `field ?? null` on an omitempty field: the parser left a
// zero out, so the api saw nothing there and wrote null.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullFloat(v float32) *float32 {
	if v == 0 {
		return nil
	}
	return &v
}

func nullInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

// orEmpty is `?? []`: an array the parser left out, or sent as null, is empty.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// BuildPlayback assembles the playback blob for one demo of a match map.
func BuildPlayback(res *Result, matchMapID string) *PlaybackBlob {
	b := &PlaybackBlob{
		SchemaVersion:       PlaybackSchemaVersion,
		ParserSchemaVersion: res.SchemaVersion,
		MatchMapID:          matchMapID,
		TickRate:            res.TickRate,
		TotalTicks:          res.TotalTicks,
		MapName:             res.MapName,
		RoundTicks:          orEmpty(res.RoundTicks),
		Players:             orEmpty(res.Players),
		Kills:               orEmpty(res.Kills),
		Bombs:               orEmpty(res.Bombs),
		KitDrops:            orEmpty(res.KitDrops),
		Positions:           make([]playbackPosition, len(res.Positions)),
		ShotsFired:          make([]playbackShot, len(res.ShotsFired)),
		GrenadeThrows:       make([]playbackGrenade, 0, len(res.GrenadeThrows)+len(res.GrenadeDetonations)),
		GrenadeTrajectories: orEmpty(res.GrenadeTrajectories),
		SmokeVolumes:        orEmpty(res.SmokeVolumes),
		SmokeVoxels:         orEmpty(res.SmokeVoxels),
		Infernos:            orEmpty(res.Infernos),
		Damages:             make([]playbackDamage, len(res.Damages)),
		Flashes:             make([]playbackFlash, len(res.Flashes)),
		RoundInventory:      make([]playbackInventory, len(res.RoundInventory)),
	}

	for i, p := range res.Positions {
		b.Positions[i] = playbackPosition{
			Round:        p.Round,
			Tick:         p.Tick,
			Attacker:     nullString(p.AttackerSteamID),
			Team:         nullString(p.Team),
			Alive:        p.Alive,
			X:            p.X,
			Y:            p.Y,
			Z:            p.Z,
			Yaw:          nullFloat(p.Yaw),
			Pitch:        nullFloat(p.Pitch),
			Health:       nullInt(p.Health),
			Armor:        nullInt(p.Armor),
			Helmet:       p.HasHelmet,
			HasBomb:      p.HasBomb,
			HasDefuser:   p.HasDefuser,
			ActiveWeapon: nullString(p.ActiveWeapon),
			Ducked:       p.Ducked,
		}
	}

	// The shot's geometry is already pointers, nil exactly when the parser
	// left it out.
	for i, s := range res.ShotsFired {
		b.ShotsFired[i] = playbackShot{
			Round:    s.Round,
			Tick:     s.Tick,
			Attacker: nullString(s.AttackerSteamID),
			Team:     nullString(s.AttackerTeam),
			With:     nullString(s.Weapon),
			Yaw:      s.Yaw,
			Pitch:    s.Pitch,
			EyeX:     s.EyeX,
			EyeY:     s.EyeY,
			EyeZ:     s.EyeZ,
			Result:   nullString(s.Result),
			ImpactX:  s.ImpactX,
			ImpactY:  s.ImpactY,
			ImpactZ:  s.ImpactZ,
			MissX:    s.MissX,
			MissY:    s.MissY,
			MissZ:    s.MissZ,
		}
	}

	// Throws then detonations, one list, told apart by phase. A throw is placed
	// at its origin; a detonation, which carries no thrower team, where it went
	// off.
	for _, g := range res.GrenadeThrows {
		b.GrenadeThrows = append(b.GrenadeThrows, playbackGrenade{
			Round:     g.Round,
			Tick:      g.Tick,
			GrenadeID: nullInt(g.GrenadeID),
			Thrower:   nullString(g.ThrowerSteamID),
			Team:      nullString(g.ThrowerTeam),
			Type:      g.Type,
			Phase:     "thrown",
			X:         g.OriginX,
			Y:         g.OriginY,
			Z:         g.OriginZ,
		})
	}
	for _, g := range res.GrenadeDetonations {
		b.GrenadeThrows = append(b.GrenadeThrows, playbackGrenade{
			Round:     g.Round,
			Tick:      g.Tick,
			GrenadeID: nullInt(g.GrenadeID),
			Thrower:   nullString(g.ThrowerSteamID),
			Type:      g.Type,
			Phase:     "detonated",
			X:         g.X,
			Y:         g.Y,
			Z:         g.Z,
		})
	}

	for i, d := range res.Damages {
		b.Damages[i] = playbackDamage{
			Round:       d.Round,
			Time:        strconv.Itoa(d.Tick),
			Attacker:    nullString(d.AttackerSteamID),
			Victim:      nullString(d.VictimSteamID),
			Weapon:      nullString(d.Weapon),
			Hitgroup:    d.Hitgroup,
			Damage:      d.Damage,
			DamageArmor: d.DamageArmor,
			Health:      nullInt(d.Health),
		}
	}

	for i, f := range res.Flashes {
		b.Flashes[i] = playbackFlash{
			Round:     f.Round,
			Tick:      f.Tick,
			Attacker:  nullString(f.AttackerSteamID),
			Victim:    nullString(f.VictimSteamID),
			Duration:  f.Duration,
			TeamFlash: f.TeamFlash,
		}
	}

	for i, r := range res.RoundInventory {
		b.RoundInventory[i] = playbackInventory{
			Round:     r.Round,
			SteamID:   nullString(r.AttackerSteamID),
			Team:      nullString(r.Team),
			Flash:     r.Flash,
			Smoke:     r.Smoke,
			HE:        r.HE,
			Molotov:   r.Molotov,
			Decoy:     r.Decoy,
			Primary:   nullString(r.Primary),
			Secondary: nullString(r.Secondary),
			Armor:     r.Armor,
			Helmet:    r.Helmet,
			Kit:       r.Kit,
		}
	}
	return b
}
