package parser

import (
	"testing"

	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
)

// fire and hurt stand in for onWeaponFire and onPlayerHurt: the bookkeeping
// each leaves behind, without a demo.
func fire(s *state, attacker string, tick int, spray bool) int {
	s.res.ShotsFired = append(s.res.ShotsFired, EventShotFired{
		Tick:            tick,
		AttackerSteamID: attacker,
		IsSpray:         spray,
		EnemySpotted:    true,
		EyeX:            f32ptr(0),
		EyeY:            f32ptr(0),
		EyeZ:            f32ptr(64),
	})
	idx := len(s.res.ShotsFired) - 1
	s.lastShot[attacker] = shotMark{tick: tick, idx: idx}
	return idx
}

func hurt(s *state, attacker string, tick, hitGroup int) int {
	s.res.Damages = append(s.res.Damages, EventDamage{Tick: tick, AttackerSteamID: attacker})
	idx := len(s.res.Damages) - 1
	s.pendingHits = append(s.pendingHits, pendingHit{
		damage:   idx,
		attacker: attacker,
		tick:     tick,
		hitGroup: hitGroup,
		victim:   r3.Vector{X: 500},
	})
	return idx
}

func shotState() *state {
	return &state{res: &Result{}, tickRate: testRate, lastShot: map[string]shotMark{}}
}

// Within a tick the engine reports the damage before the shot. The damage must
// still land on that shot, not the miss before it.
func TestDamageClaimsTheShotLaterInItsTick(t *testing.T) {
	s := shotState()
	miss := fire(s, "a", 100, false)
	dmg := hurt(s, "a", 108, 2)
	shot := fire(s, "a", 108, true)
	s.resolveHits()

	if got := s.res.ShotsFired[miss].Result; got != "" {
		t.Errorf("the earlier miss became %q", got)
	}
	sh := s.res.ShotsFired[shot]
	if sh.Result != "hit" {
		t.Fatalf("the same-tick shot is %q, want hit", sh.Result)
	}
	if sh.ImpactX == nil || *sh.ImpactX >= 500 {
		t.Errorf("impact should sit on the near side of the victim, got %v", sh.ImpactX)
	}
	if !s.res.Damages[dmg].FromSpray {
		t.Error("the damage should carry its own shot's spray flag")
	}
}

func TestHeadshotDamageMarksAHeadshot(t *testing.T) {
	s := shotState()
	hurt(s, "a", 50, 1)
	shot := fire(s, "a", 50, false)
	s.resolveHits()
	if got := s.res.ShotsFired[shot].Result; got != "headshot" {
		t.Fatalf("shot is %q, want headshot", got)
	}
}

// One bullet through two players: the second victim belongs to the same shot
// and must not turn the attacker's earlier miss into a hit.
func TestSecondVictimOfOneBulletDoesNotStealAMiss(t *testing.T) {
	s := shotState()
	miss := fire(s, "a", 100, false)
	hurt(s, "a", 108, 2)
	hurt(s, "a", 108, 3)
	shot := fire(s, "a", 108, true)
	s.resolveHits()

	if got := s.res.ShotsFired[shot].Result; got != "hit" {
		t.Fatalf("the bullet is %q, want hit", got)
	}
	if got := s.res.ShotsFired[miss].Result; got != "" {
		t.Fatalf("the earlier miss became %q", got)
	}
}

// Damage reported a tick or two after its shot falls back to the latest shot
// inside the spray window, and to nothing beyond it.
func TestDamageWithoutASameTickShot(t *testing.T) {
	s := shotState()
	shot := fire(s, "a", 100, false)
	hurt(s, "a", 102, 2)
	s.resolveHits()
	if got := s.res.ShotsFired[shot].Result; got != "hit" {
		t.Fatalf("late damage left its shot as %q, want hit", got)
	}

	s = shotState()
	shot = fire(s, "a", 100, false)
	hurt(s, "a", 100+int(sprayWindowSecs*testRate), 2)
	s.resolveHits()
	if got := s.res.ShotsFired[shot].Result; got != "" {
		t.Fatalf("a shot outside the window became %q", got)
	}
}

func TestOnlyBulletsClaimShots(t *testing.T) {
	for _, c := range []struct {
		weapon *common.Equipment
		bullet bool
	}{
		{nil, false},
		{&common.Equipment{Type: common.EqGlock}, true},
		{&common.Equipment{Type: common.EqMP5}, true},
		{&common.Equipment{Type: common.EqAK47}, true},
		{&common.Equipment{Type: common.EqNova}, true},
		{&common.Equipment{Type: common.EqHE}, false},
		{&common.Equipment{Type: common.EqMolotov}, false},
		{&common.Equipment{Type: common.EqIncendiary}, false},
		{&common.Equipment{Type: common.EqKnife}, false},
		{&common.Equipment{Type: common.EqZeus}, false},
	} {
		name := "nil"
		if c.weapon != nil {
			name = c.weapon.Type.String()
		}
		if got := isBulletWeapon(c.weapon); got != c.bullet {
			t.Errorf("%s: bullet = %v, want %v", name, got, c.bullet)
		}
	}
}

// Every damage in test.dem has a same-tick shot from its attacker, and every
// kill is a single shot that connected.
func TestTestDemoKillShotsAreHits(t *testing.T) {
	res := parseTestDemo(t)
	shotAt := func(attacker string, tick int) *EventShotFired {
		for i := range res.ShotsFired {
			if sh := &res.ShotsFired[i]; sh.AttackerSteamID == attacker && sh.Tick == tick {
				return sh
			}
		}
		return nil
	}
	if len(res.Kills) != 12 {
		t.Fatalf("%d kills, want 12", len(res.Kills))
	}
	for _, k := range res.Kills {
		sh := shotAt(k.KillerSteamID, k.Tick)
		if sh == nil {
			t.Fatalf("kill at %d has no shot from its killer", k.Tick)
		}
		want := "hit"
		if k.Headshot {
			want = "headshot"
		}
		if sh.Result != want || sh.ImpactX == nil {
			t.Errorf("kill shot at %d (%s) is %q, want %q with an impact", k.Tick, k.Weapon, sh.Result, want)
		}
	}
	hits := 0
	for _, sh := range res.ShotsFired {
		if sh.Result != "" {
			hits++
		}
	}
	for _, d := range res.Damages {
		if sh := shotAt(d.AttackerSteamID, d.Tick); sh == nil || sh.Result == "" {
			t.Errorf("damage at %d did not mark its own shot", d.Tick)
		}
	}
	if hits != len(res.Damages) {
		t.Errorf("%d shots hit for %d damages; each damage here is its own bullet", hits, len(res.Damages))
	}
}
