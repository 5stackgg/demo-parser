package parser

import "sort"

func (s *state) computeTrades() {
	rate := s.parser.TickRate()
	if rate <= 0 {
		rate = 64
	}
	window := int(rate * 3)

	rounds := make([]RoundTick, len(s.res.RoundTicks))
	copy(rounds, s.res.RoundTicks)
	sort.Slice(rounds, func(i, j int) bool { return rounds[i].StartTick < rounds[j].StartTick })
	roundOf := func(tick int) int {
		r := 0
		for _, rt := range rounds {
			if rt.StartTick <= tick {
				r = rt.Round
			} else {
				break
			}
		}
		return r
	}

	teamByRound := map[int]map[string]string{}
	setTeam := func(round int, sid, team string) {
		if sid == "" || team == "" {
			return
		}
		if teamByRound[round] == nil {
			teamByRound[round] = map[string]string{}
		}
		if _, ok := teamByRound[round][sid]; !ok {
			teamByRound[round][sid] = team
		}
	}
	for _, ri := range s.res.RoundInventory {
		setTeam(ri.Round, ri.AttackerSteamID, ri.Team)
	}
	for _, k := range s.res.Kills {
		r := roundOf(k.Tick)
		setTeam(r, k.KillerSteamID, k.KillerTeam)
		setTeam(r, k.VictimSteamID, k.VictimTeam)
	}
	for _, sp := range s.res.Spotted {
		setTeam(sp.Round, sp.SpotterSteamID, sp.SpotterTeam)
	}

	spotted := map[[2]string][]int{}
	for _, sp := range s.res.Spotted {
		key := [2]string{sp.SpotterSteamID, sp.SpottedSteamID}
		spotted[key] = append(spotted[key], sp.Tick)
	}
	for key := range spotted {
		sort.Ints(spotted[key])
	}
	sawWithin := func(a, b string, lo, hi int) bool {
		arr := spotted[[2]string{a, b}]
		i := sort.SearchInts(arr, lo)
		return i < len(arr) && arr[i] <= hi
	}

	kills := make([]EventKill, len(s.res.Kills))
	copy(kills, s.res.Kills)
	sort.SliceStable(kills, func(i, j int) bool { return kills[i].Tick < kills[j].Tick })

	type agg struct {
		tkOpp, tkSucc, tdOpp, tdSucc int
		utilOnDeathSum, deaths       int
	}
	stats := map[string]*agg{}
	get := func(sid string) *agg {
		if stats[sid] == nil {
			stats[sid] = &agg{}
		}
		return stats[sid]
	}

	deadByRound := map[int]map[string]bool{}
	for _, k := range kills {
		r := roundOf(k.Tick)
		if deadByRound[r] == nil {
			deadByRound[r] = map[string]bool{}
		}

		if k.VictimSteamID != "" {
			a := get(k.VictimSteamID)
			a.deaths++
			if k.VictimUtilityValue != nil {
				a.utilOnDeathSum += *k.VictimUtilityValue
			}
		}

		enemyKill := k.KillerSteamID != "" && k.VictimSteamID != "" &&
			k.KillerTeam != "" && k.VictimTeam != "" && k.KillerTeam != k.VictimTeam
		if enemyKill {
			killer, victim, t := k.KillerSteamID, k.VictimSteamID, k.Tick
			vteam := teamByRound[r][victim]

			losMates := []string{}
			for sid, team := range teamByRound[r] {
				if sid == victim || team != vteam || deadByRound[r][sid] {
					continue
				}
				if sawWithin(sid, killer, t-window, t+window) || sawWithin(killer, sid, t-window, t+window) {
					losMates = append(losMates, sid)
				}
			}

			tradedBy := map[string]bool{}
			for _, k2 := range kills {
				if k2.Tick <= t || k2.Tick > t+window || roundOf(k2.Tick) != r {
					continue
				}
				if k2.VictimSteamID == killer && teamByRound[r][k2.KillerSteamID] == vteam {
					tradedBy[k2.KillerSteamID] = true
				}
			}

			for _, mate := range losMates {
				get(mate).tkOpp++
			}
			for trader := range tradedBy {
				get(trader).tkSucc++
			}
			if len(losMates) > 0 {
				get(victim).tdOpp++
				if len(tradedBy) > 0 {
					get(victim).tdSucc++
				}
			}
		}

		if k.VictimSteamID != "" {
			deadByRound[r][k.VictimSteamID] = true
		}
	}

	ids := make([]string, 0, len(stats))
	for sid := range stats {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	for _, sid := range ids {
		a := stats[sid]
		s.res.PlayerTrades = append(s.res.PlayerTrades, PlayerTrade{
			SteamID:                  sid,
			TradeKillOpportunities:   a.tkOpp,
			TradeKillAttempts:        a.tkOpp,
			TradeKillSuccesses:       a.tkSucc,
			TradedDeathOpportunities: a.tdOpp,
			TradedDeathAttempts:      a.tdOpp,
			TradedDeathSuccesses:     a.tdSucc,
			UtilOnDeathSum:           a.utilOnDeathSum,
			Deaths:                   a.deaths,
		})
	}
}
