# Leetify-parity Match Stats — Progress

Cross-repo workstream tracked here because the demo-parser is the
gating dependency for the AIM-stats half. The full plan lives at
`~/.claude/plans/demo-parser-api-web-i-curried-nebula.md`.

## What the user asked for

Leetify-style match analytics on the existing match page (`web/pages/matches/[id].vue`):

- **AIM**: spotted accuracy, time-to-damage, crosshair placement, head accuracy %, HS kill %, spray accuracy, counter-strafing, overall accuracy.
- **Trades**: trade kill opportunities/attempts/success, traded death opportunities/attempts/success.
- **Utility**: quality / quantity rating (flash assists, enemies/friends flashed, avg blind time, avg HE damage, utility usage counts, unused $).

User decisions during planning:

- Full Leetify parity (not just trades).
- Frame-level demo parsing **is** in scope (for crosshair placement + counter-strafing).
- Three new tabs in `MatchTabs.vue`.
- 5-second trade window.

## Phasing

| Phase | Scope | Status |
|-------|-------|--------|
| 1 | Trade stats (SQL + UI, no demo-parser changes) | **Done** |
| 2 | Demo-parser event extensions + AIM tab v1 (no frame data) | **Done** |
| 3 | Frame-pass for crosshair placement + counter-strafing | Not started |
| 4 | Utility quality/quantity rating + advanced utility tab | Not started |
| 5 | Polish (i18n for all locales, tooltips, deprecate legacy `utility` tab) | Not started |

## Phase 1 — Trade Stats (shipped)

All inputs already in `player_kills`. No demo-parser changes.

**Files touched:**

- `api/hasura/migrations/default/1790000000000_player_match_map_stats_trade_columns/{up,down}.sql` — 5 columns: `trade_kill_opportunities`, `trade_kill_attempts`, `trade_kill_successes`, `traded_death_opportunities`, `traded_death_successes`.
- `api/hasura/functions/stats/recompute_player_match_map_stats.sql` — added `player_round_team`, `trade_pairs`, `trade_kill_agg`, `trade_kill_opp_agg`, `traded_death_agg`, `traded_death_opp_agg` CTEs. `time` is `timestamptz`, so the 5s window uses `interval '5 seconds'`.
- `api/hasura/views/player_match_stats_v.sql` — cross-map sums.
- `api/hasura/metadata/databases/default/tables/public_player_match_map_stats.yaml` + `public_player_match_stats_v.yaml` — column allow-list.
- `web/graphql/matchMapStatsGraphql.ts` + `matchAllMapsStatsGraphql.ts` — fragments extended.
- `web/components/match/LineupTradeStats.vue` — new component (Trade Kill Opps / Attempts / Trade Kill % / Traded Death Opps / Traded Death % / Net Trade).
- `web/components/match/MatchTabs.vue` — `trade-stats` tab added to mobile select, desktop list, content block, `availableMatchTabs`, and the activeMap watcher's stats-tab list.
- `web/i18n/locales/en.json` — `tabs.trade_stats` + 6 stat keys.

**Definition:** A "trade" is a kill where the trader's victim was the player who killed a teammate of the trader, within 5 seconds. Under this definition `trade_kill_attempts == trade_kill_successes` (Leetify exposes both for symmetry). Trade kill opportunities = times a teammate of yours was killed while you were alive.

## Phase 2 — Demo-parser event extensions + AIM tab v1 (shipped, untested on real demo)

### Demo-parser changes (this repo)

`internal/parser/parser.go`:

- New struct types: `EventShotFired`, `EventDamage`, `EventSpotted`, `EventGrenadeThrow`, `EventGrenadeDetonate`. All wired into `Result` as `omitempty` slices (back-compat JSON).
- Shared closure state: `currentRound`, `currentRoundStartTick`, `seenSpotters` (rising-edge spotter cache keyed by spotted SteamID).
- New event handlers:
  - `events.WeaponFire` → firearm-only via `e.Weapon.Class()` against `EqClassPistols`/`EqClassSMG`/`EqClassHeavy`/`EqClassRifle`. Drops knives, grenades, equipment.
  - `events.PlayerHurt` → uses `e.Player`, `e.Attacker`, `e.HitGroup`, `e.HealthDamage`, `e.ArmorDamage`. Stamps `SinceRoundStart = (tick - currentRoundStartTick) / TickRate()`.
  - `events.PlayerSpottersChanged` → iterates participants, emits one `EventSpotted` per spotter that just appeared in `e.Spotted`'s spotter set. Cache reset on `RoundStart`.
  - `events.GrenadeProjectileThrow` → position from `e.Projectile.Position()`, thrower from `e.Projectile.Thrower` (falls back to `Owner`), type from `e.Projectile.WeaponInstance.Type` via `grenadeTypeCode`.
  - `events.HeExplode`, `events.FlashExplode`, `events.SmokeStart`, `events.FireGrenadeStart` → all carry `GrenadeEvent` (Position + Thrower + GrenadeType). Mollies have **nil thrower** in Source 2 demos — see gotchas.
- `RoundStart` handler updated to stamp `currentRoundStartTick` and clear `seenSpotters`.

`cmd/server/main.go`: CLI summary line includes the new counts (shots/dmg/spotted/thrown/detonated).

**Build status:** `go build ./...` and `go vet ./...` clean. **Not yet tested against a real `.dem`** — no test file was available locally.

### API ingestion (5stack/api)

- `src/demos/demo-parser.service.ts` — `ParsedDemo` extended with `shots_fired`, `damages`, `spotted`, `grenade_throws`, `grenade_detonations` plus typed shapes.
- `src/demos/demo-metadata.service.ts` — `parseAndPersist` now:
  1. Persists the existing demo-metadata mutation.
  2. Calls `persistDemoEvents()` which `DELETE`s prior rows for the `match_map_id` then bulk-inserts new ones in 1000-row chunks inside a transaction (uses `PostgresService`, not Hasura — too many params for GQL).
  3. Calls `runRecompute()` → `SELECT public.recompute_player_match_map_stats($1::uuid)`.
- `src/demos/demos.module.ts` — `PostgresModule` added to imports.
- TypeScript clean on changed files (`yarn tsc --noEmit` shows only pre-existing test-spec errors).

### Hasura schema (5stack/api)

- Migration `1791000000000_demo_event_tables/up.sql` creates:
  - `player_shots_fired (id, match_id, match_map_id, round, tick, attacker_steam_id, attacker_team, with)`
  - `player_spotted (id, match_id, match_map_id, round, tick, spotter_steam_id, spotted_steam_id, spotter_team)`
  - `player_grenade_throws (id, match_id, match_map_id, round, tick, thrower_steam_id, thrower_team, type, phase, x, y, z)` — `phase` ∈ `'thrown' | 'detonated'`, so throws + detonations share one table. Throw rows carry `ox/oy/oz` collapsed into `x/y/z`.
- Migration `1792000000000_player_match_map_stats_aim_columns/up.sql` adds 12 columns: `shots_fired`, `hits`, `headshot_hits`, `time_to_damage_sum_s`, `time_to_damage_count`, `spotted_count`, `spotted_with_damage_count`, `he_throws`, `molotov_throws`, `smoke_throws`, `decoy_throws`, `rounds_played`.
- `recompute_player_match_map_stats.sql` extended with `shots_agg`, `hits_agg`, `ttd_per_round` + `ttd_agg`, `spotted_agg`, `throws_agg`, `rounds_played_const`. `player_set` widened to include players who appear only in demo-event tables (shots-only / spotted-only / grenade-only players).
- `player_match_stats_v.sql` — cross-map sums + `avg_time_to_damage_s` derived column.
- Hasura metadata yamls for the 3 new tables + manifest entry, plus extended `select_permissions` on `player_match_map_stats` and `player_match_stats_v`.

### Web UI (5stack/web)

- `components/match/LineupAimStats.vue` — Accuracy / Head Accuracy / HS Kill % / Time-to-Damage / Spotted Acc, plus two `—` placeholder columns (Crosshair Placement, Counter-Strafing) that light up in Phase 3.
- `graphql/matchMapStatsGraphql.ts` + `matchAllMapsStatsGraphql.ts` — extended with AIM columns.
- `components/match/MatchTabs.vue` — `aim-stats` tab in mobile select + desktop list + content + `availableMatchTabs` + watcher's stats-tabs list.
- `i18n/locales/en.json` — `tabs.aim_stats` + 7 column keys.

## Gotchas (from the demo-parser implementation pass)

1. **Molotov/incendiary detonations have nil thrower** in Source 2 demos. `EventGrenadeDetonate.ThrowerSteamID` will be empty for mollies. The ingestion side should attribute by joining back to the prior `EventGrenadeThrow` (same `match_map_id`, `round`, `type='Molotov'`). The unified `phase` column on `player_grenade_throws` makes this a single-table query.
2. **PlayerSpottersChanged is rising-edge only** by design. We track `seenSpotters[spottedSteamID] = set<spotterSteamID>`, reset on `RoundStart` (visibility resets at freeze-break anyway). No "un-spotted" events; durations have to come from elsewhere.
3. **`since_round_start` falls back to 0.0** if `TickRate()` is 0 — only fails on demos without an observed tick rate.
4. **`spotted_with_damage_count` is round-scoped, not tick-scoped.** Demo ticks aren't directly comparable to GSI's wall-clock `player_damages.time`, so v1 widens to "spotter→spotted damage anywhere in the same round". Tighten this to a tick window once demo ingestion also writes `player_damages.tick`.
5. **`shots_fired` filter drops knives + grenades.** If a future tab wants "shots + nades thrown" totals, union `player_shots_fired` with `player_grenade_throws` on the api side.
6. **Pre-existing TS errors in the api repo** are in `matchmaking/matchmake.service.spec.ts` and `match-server-middleware.middleware.spec.ts`. Not from this work.

## To ship Phase 1 + Phase 2

1. Restart the api. Migrations apply on boot; the changed `recompute_player_match_map_stats.sql` + `player_match_stats_v.sql` files re-apply on digest change, triggering `recompute_all_player_match_map_stats()` as a backfill (this populates trade columns for every existing finished match).
2. `cd web && yarn codegen` against a Hasura instance with the new schema. Without this the zeus types won't know the new columns and the new fragment selectors won't typecheck.
3. Build + deploy the new demo-parser image.
4. (Optional) Re-parse historical demos via the existing `reparseById` endpoint to populate AIM columns for past matches. Trades work without this.

## Verification

```sql
-- Trades populated for a finished match
SELECT steam_id, trade_kill_attempts, trade_kill_successes,
       traded_death_opportunities, traded_death_successes
  FROM player_match_map_stats
 WHERE match_map_id = '<finished-match-map-id>';

-- After a demo re-parse
SELECT COUNT(*) FROM public.player_shots_fired      WHERE match_map_id = '<id>'; -- expect 20–50k
SELECT COUNT(*) FROM public.player_spotted          WHERE match_map_id = '<id>'; -- expect 1–3k
SELECT COUNT(*) FROM public.player_grenade_throws   WHERE match_map_id = '<id>'; -- expect 500–1500

SELECT steam_id, shots_fired, hits, headshot_hits,
       time_to_damage_sum_s / NULLIF(time_to_damage_count, 0) AS avg_ttd_s,
       spotted_count, spotted_with_damage_count
  FROM player_match_map_stats
 WHERE match_map_id = '<id>';
```

Open a finished match in the web UI — Trades + Aim tabs render. Aim's Crosshair / Counter-Strafing columns show `—` until Phase 3.

To smoke-test the demo-parser itself before deploying:

```sh
cd demo-parser
go build -o ./bin/parser ./cmd/server
./bin/parser parse < some.dem | jq '{
  shots: (.shots_fired | length),
  dmg:   (.damages | length),
  spot:  (.spotted | length),
  thr:   (.grenade_throws | length),
  det:   (.grenade_detonations | length)
}'
```

## Phase 3 — Frame Pass (not started)

Plan:

- Register `events.FrameDone`; maintain `map[steamID64]playerFrame{pos, vel, eyeYaw, eyePitch}` snapshot from `parser.GameState().Participants().Playing()`.
- On `WeaponFire`, look up shooter's current frame and compute:
  - `Speed = sqrt(velX² + velY²)`
  - `CounterStrafed = Speed < 5` u/s (CS2 movement-accuracy threshold)
  - `CrosshairAngleNearestEnemyDeg` = angle between shooter eye vector and vector to nearest alive enemy.
- Annotate `EventShotFired` (don't emit per-tick rows — ~1.1M/match would crush Hasura).
- `ALTER TABLE player_shots_fired ADD COLUMN speed numeric, counter_strafed bool, crosshair_angle_deg numeric;`
- `player_match_map_stats` += `counter_strafed_shots`, `spray_shots`, `crosshair_angle_sum_deg`, `crosshair_angle_count`. `spray_shots` = consecutive shots within 0.25s (LAG over `player_shots_fired`).
- Wire the placeholder columns in `LineupAimStats.vue`.

**Perf risk:** frame handler iterating participants every tick on a 40-min demo. Benchmark before committing. Fallback: sample every N ticks, or only snapshot near WeaponFire.

## Phase 4 — Utility Quality/Quantity (not started)

- `player_match_map_stats` += `flash_quality_sum`, `flash_quality_count`.
- CTE on `player_flashes` filtered `team_flash = false`, summed by attacker.
- New component `LineupUtilityAdvanced.vue` + `utility-advanced` tab. Existing simple `utility` tab kept for one release.

## Phase 5 — Polish (not started)

- i18n keys in all 17 locales (currently only `en.json`; Nuxt i18n falls back to English for missing keys, so the other locales render correctly until translated).
- Tooltips citing Leetify definitions.
- Deprecate legacy `utility` tab once `utility-advanced` is stable.

## Task tracking

When you resume, the eight tasks from this session are all `completed`. Re-create the task list for Phases 3-5 when you pick this up again. Key files to revisit:

- `demo-parser/internal/parser/parser.go` — for Phase 3 frame handler.
- `api/hasura/functions/stats/recompute_player_match_map_stats.sql` — every new metric extends this.
- `web/components/match/MatchTabs.vue` — new tab wiring (mobile + desktop + content + availableMatchTabs + watcher stats list).
- `web/components/match/LineupAimStats.vue` — Phase 3 fills in the two placeholder columns.
