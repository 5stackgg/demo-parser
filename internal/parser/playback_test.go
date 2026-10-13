package parser

import (
	"encoding/json"
	"testing"
)

// The blob must read exactly as the api's buildPlaybackBlob wrote it from this
// parser's JSON, where an omitempty zero is absent and became null, 0 or [] by
// the api's `??`. Each row below has its zero-valued twin, so both sides of
// every one of those defaults are pinned.
func TestBuildPlaybackMatchesTheApiMapping(t *testing.T) {
	f := func(v float32) *float32 { return &v }
	res := &Result{
		SchemaVersion: 4,
		TotalTicks:    1000,
		TickRate:      64,
		MapName:       "de_anubis",
		Positions: []EventPosition{
			{Tick: 10, Round: 2, AttackerSteamID: "7656", Team: "ct", Alive: true, X: 1.5, Y: -2, Z: 3,
				Yaw: 90, Pitch: -10, Health: 100, Armor: 50, HasHelmet: true, HasBomb: true,
				HasDefuser: true, ActiveWeapon: "ak47", Ducked: true},
			{Tick: 11},
		},
		ShotsFired: []EventShotFired{
			{Tick: 20, Round: 3, AttackerSteamID: "7656", AttackerTeam: "t", Weapon: "awp",
				Yaw: f(0), Pitch: f(1), EyeX: f(2), EyeY: f(3), EyeZ: f(4), Result: "hit",
				ImpactX: f(5), ImpactY: f(6), ImpactZ: f(7), MissX: f(8), MissY: f(9), MissZ: f(10)},
			{Tick: 21},
		},
		GrenadeThrows: []EventGrenadeThrow{
			{Tick: 30, Round: 1, GrenadeID: 7, ThrowerSteamID: "7656", ThrowerTeam: "ct", Type: "Smoke", OriginX: 1, OriginY: 2, OriginZ: 3},
			{Tick: 31, Type: "Flash"},
		},
		GrenadeDetonations: []EventGrenadeDetonate{
			{Tick: 40, Round: 1, GrenadeID: 7, ThrowerSteamID: "7656", Type: "Smoke", X: 4, Y: 5, Z: 6},
		},
		Damages: []EventDamage{
			{Tick: 50, Round: 4, AttackerSteamID: "a", VictimSteamID: "v", Weapon: "m4a1", Damage: 27, DamageArmor: 3, Hitgroup: 1, Health: 73},
			{Tick: 51, Damage: 0},
		},
		Flashes: []EventFlash{
			{Tick: 60, Round: 5, AttackerSteamID: "a", VictimSteamID: "v", Duration: 2.25, TeamFlash: true},
			{Tick: 61},
		},
		RoundInventory: []EventRoundInventory{
			{Round: 6, AttackerSteamID: "a", Team: "t", Flash: 1, Smoke: 1, HE: 1, Molotov: 1, Decoy: 1,
				Primary: "ak47", Secondary: "glock", Armor: 100, Helmet: true, Kit: true},
			{},
		},
	}
	got, err := json.Marshal(BuildPlayback(res, "mm-1"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":11,"parser_schema_version":4,"match_map_id":"mm-1","tick_rate":64,"total_ticks":1000,"map_name":"de_anubis",` +
		`"round_ticks":[],"players":[],"kills":[],"bombs":[],"kit_drops":[],` +
		`"positions":[` +
		`{"round":2,"tick":10,"attacker_steam_id":"7656","attacker_team":"ct","alive":true,"x":1.5,"y":-2,"z":3,"yaw":90,"pitch":-10,"health":100,"armor":50,"helmet":true,"has_bomb":true,"has_defuser":true,"active_weapon":"ak47","ducked":true},` +
		`{"round":0,"tick":11,"attacker_steam_id":null,"attacker_team":null,"alive":false,"x":0,"y":0,"z":0,"yaw":null,"pitch":null,"health":null,"armor":null,"helmet":false,"has_bomb":false,"has_defuser":false,"active_weapon":null,"ducked":false}],` +
		`"shots_fired":[` +
		`{"round":3,"tick":20,"attacker_steam_id":"7656","attacker_team":"t","with":"awp","yaw":0,"pitch":1,"eye_x":2,"eye_y":3,"eye_z":4,"result":"hit","impact_x":5,"impact_y":6,"impact_z":7,"miss_x":8,"miss_y":9,"miss_z":10},` +
		`{"round":0,"tick":21,"attacker_steam_id":null,"attacker_team":null,"with":null,"yaw":null,"pitch":null,"eye_x":null,"eye_y":null,"eye_z":null,"result":null,"impact_x":null,"impact_y":null,"impact_z":null,"miss_x":null,"miss_y":null,"miss_z":null}],` +
		`"grenade_throws":[` +
		`{"round":1,"tick":30,"grenade_id":7,"thrower_steam_id":"7656","thrower_team":"ct","type":"Smoke","phase":"thrown","x":1,"y":2,"z":3},` +
		`{"round":0,"tick":31,"grenade_id":null,"thrower_steam_id":null,"thrower_team":null,"type":"Flash","phase":"thrown","x":0,"y":0,"z":0},` +
		`{"round":1,"tick":40,"grenade_id":7,"thrower_steam_id":"7656","thrower_team":null,"type":"Smoke","phase":"detonated","x":4,"y":5,"z":6}],` +
		`"grenade_trajectories":[],"smoke_volumes":[],"smoke_voxels":[],"infernos":[],` +
		`"damages":[` +
		`{"round":4,"time":"50","attacker_steam_id":"a","attacked_steam_id":"v","weapon":"m4a1","hitgroup":1,"damage":27,"damage_armor":3,"health":73},` +
		`{"round":0,"time":"51","attacker_steam_id":null,"attacked_steam_id":null,"weapon":null,"hitgroup":0,"damage":0,"damage_armor":0,"health":null}],` +
		`"flashes":[` +
		`{"round":5,"tick":60,"attacker_steam_id":"a","attacked_steam_id":"v","duration":2.25,"team_flash":true},` +
		`{"round":0,"tick":61,"attacker_steam_id":null,"attacked_steam_id":null,"duration":0,"team_flash":false}],` +
		`"round_inventory":[` +
		`{"round":6,"steam_id":"a","team":"t","flash":1,"smoke":1,"he":1,"molotov":1,"decoy":1,"primary":"ak47","secondary":"glock","armor":100,"helmet":true,"kit":true},` +
		`{"round":0,"steam_id":null,"team":null,"flash":0,"smoke":0,"he":0,"molotov":0,"decoy":0,"primary":null,"secondary":null,"armor":0,"helmet":false,"kit":false}]}`
	if string(got) != want {
		t.Fatalf("blob differs from the api's mapping:\n got  %s\n want %s", got, want)
	}
}
