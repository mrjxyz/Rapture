package world

import (
	"time"

	"github.com/Spelunkern/Rapture/internal/packet"
)

// Guild duels follow CUser::PacketPVPGuild 0x0047da70 and CGuild::PvP*:
//
//   - 0x2501 (target id) from a member of rank 1-4 whose guild and self
//     are free: the target is a rank 1-4 member of another free guild on
//     the zone, not dueling; else 0x2502 result 2. Both get 0x2501
//     (requester, target); the two are their guilds' duel leaders, the
//     guilds states 1 and 2, for 10 s (then 0x2502 3).
//   - 0x2502 (u8) from the target: 0 refuses (0x2502 0 to the requester);
//     else both guilds count down 5 s from the midpoint of the two
//     leaders, CGuild::PvPUserSet enrolls each guild's online members on
//     the map within 40 units of it (their damage ledgers reset), the
//     requester gets 0x2502 1 and the participants 0x2503 (center x, z,
//     opposing guild); then state 4 and 0x2504 to them.
//   - Participants of the two guilds are foes (IsTeam) and only
//     team-usable within their side (IsTeamUsable).
//   - CGuild::PvPUserLeave: leaving the 40-unit circle, the zone or the
//     guild (reason 1) or dying (3) takes a participant out with 0x2506
//     (reason, id) to both sides; a guild left without participants, or
//     whose leader leaves for reason 1, loses: 0x2505 1 to the winners,
//     2 to the losers, and both guilds reset. A death by an opposing
//     participant (PvPGetDeathType 5) is such a leave, without rewards and
//     keeping the exp (m_nVehicleStatus -1).
//   - Before the countdown, the leaders' deaths, zone or guild leaves
//     cancel with 0x2502 3.
//
// CGuild::GuildPvPWin / GuildPvPLoss (guild points) have no caller in
// EP4.5 and are not modeled. A disband during a guild duel is not handled.

const (
	gduelNone      = 0
	gduelAsking    = 1
	gduelAsked     = 2
	gduelCountdown = 3
	gduelFighting  = 4
)

// guildDuel is CGuild +0x4b4..+0x4d4 (guarded by duelMu).
type guildDuel struct {
	state     int
	leader    uint32 // +0x4c0, this guild's duel leader
	oppLeader uint32 // +0x4b8
	oppGuild  uint32 // +0x4bc
	center    [2]float32
	mapID     uint16
	count     int
	deadline  time.Time
}

// guildPlayers snapshots a guild's online members.
func guildPlayers(g *guild) []*ConnectedPlayer {
	guildMu.Lock()
	defer guildMu.Unlock()
	var out []*ConnectedPlayer
	for _, m := range g.members {
		if m.player != nil {
			out = append(out, m.player)
		}
	}
	return out
}

// guildAndRank is the user's guild and rank (nil, 0 without one).
func guildAndRank(p *ConnectedPlayer) (*guild, uint8) {
	guildMu.Lock()
	defer guildMu.Unlock()
	g := guildOfChar[p.CharacterID]
	if g == nil {
		return nil, 0
	}
	m := g.members[p.CharacterID]
	if m == nil {
		return nil, 0
	}
	return g, m.Rank
}

func guildFromID(id uint32) *guild {
	guildMu.Lock()
	defer guildMu.Unlock()
	return guildsByID[id]
}

// sendPvPLocked is CGuild::SendPvP / SendPvPJoinUser: the participants.
func sendPvPLocked(players []*ConnectedPlayer, frame []byte, label string, joined bool) {
	for _, p := range players {
		if (joined && p.duel.gJoined) || (!joined && p.duel.gOpp != 0) {
			p.Send(frame, label)
		}
	}
}

// guildDuelFoes is IsTeam's guild-duel case.
func guildDuelFoes(a, b *ConnectedPlayer) bool {
	duelMu.Lock()
	aOpp, bOpp, aOn, bOn := a.duel.gOpp, b.duel.gOpp, a.duel.gActive, b.duel.gActive
	duelMu.Unlock()
	if aOpp == 0 || bOpp == 0 || !aOn || !bOn {
		return false
	}
	ga, gb := guildIDFor(a.CharacterID), guildIDFor(b.CharacterID)
	return ga != 0 && gb != 0 && gb == aOpp && bOpp == ga
}

// guildDuelSide is IsTeamUsable's guild-duel check: the same side, or
// neither in one.
func guildDuelSide(a, b *ConnectedPlayer) bool {
	duelMu.Lock()
	defer duelMu.Unlock()
	return a.duel.gOpp == b.duel.gOpp
}

func (s *Session) handleGuildDuel(op packet.Opcode, p []byte) {
	if s.player == nil || s.char == nil {
		return
	}
	switch op {
	case 0x2501:
		if len(p) >= 6 {
			id, _ := packet.U32(p[2:])
			s.guildDuelRequest(id)
		}
	case 0x2502:
		if len(p) >= 3 {
			s.guildDuelAnswer(p[2] != 0)
		}
	}
}

func (s *Session) guildDuelRequest(target uint32) {
	me := s.player
	g, rank := guildAndRank(me)
	if g == nil || rank > 4 {
		return
	}
	t := FindPlayerByID(target)
	var tg *guild
	var trank uint8
	if t != nil && t.MapID() == s.char.MapID {
		tg, trank = guildAndRank(t)
	}
	duelMu.Lock()
	defer duelMu.Unlock()
	if g.gd.state != gduelNone || me.duel.state != duelNone {
		return
	}
	if tg == nil || trank == 0 || trank > 4 || tg == g || t.duel.state != duelNone || tg.gd.state != gduelNone {
		s.send(duelResult(0x2502, 2, target), "0x2502 guild duel refused")
		return
	}
	deadline := time.Now().Add(duelRequestTime)
	g.gd = guildDuel{state: gduelAsking, leader: me.CharacterID, oppLeader: target, oppGuild: tg.ID, deadline: deadline}
	tg.gd = guildDuel{state: gduelAsked, leader: target, oppLeader: me.CharacterID, oppGuild: g.ID, deadline: deadline}
	me.duel.gOpp, t.duel.gOpp = tg.ID, g.ID
	frame := duelPacket(0x2501, func(w *packet.Writer) { w.U32(me.CharacterID); w.U32(target) })
	s.send(frame, "0x2501 guild duel request")
	t.Send(frame, "0x2501 guild duel request")
}

func (s *Session) guildDuelAnswer(accept bool) {
	me := s.player
	g, rank := guildAndRank(me)
	if g == nil || rank > 4 {
		return
	}
	duelMu.Lock()
	st, oppLeader, oppGuild := g.gd.state, g.gd.oppLeader, g.gd.oppGuild
	duelMu.Unlock()
	if st != gduelAsked {
		return
	}
	r := FindPlayerByID(oppLeader)
	rg := guildFromID(oppGuild)
	if r == nil || rg == nil {
		return
	}
	duelMu.Lock()
	if g.gd.state != gduelAsked {
		duelMu.Unlock()
		return
	}
	if !accept {
		g.gd, rg.gd = guildDuel{}, guildDuel{}
		me.duel.gOpp, r.duel.gOpp = 0, 0
		duelMu.Unlock()
		r.Send(duelResult(0x2502, 0, me.CharacterID), "0x2502 guild duel refused")
		return
	}
	rx, rz := r.PositionXZ()
	center := [2]float32{(s.char.PosX + rx) / 2, (s.char.PosZ + rz) / 2}
	deadline := time.Now().Add(duelCountdownTime)
	for _, x := range []*guild{g, rg} {
		x.gd.state, x.gd.center, x.gd.mapID, x.gd.deadline = gduelCountdown, center, s.char.MapID, deadline
	}
	duelMu.Unlock()
	mine, theirs := guildPlayers(g), guildPlayers(rg)
	duelMu.Lock()
	defer duelMu.Unlock()
	enroll := func(x *guild, players []*ConnectedPlayer) {
		x.gd.count = 0
		for _, p := range players {
			px, pz := p.PositionXZ()
			if p.MapID() != x.gd.mapID || DistanceXZ(px, pz, center[0], center[1]) >= duelRange {
				p.duel.gOpp, p.duel.gJoined, p.duel.gActive = 0, false, false
				continue
			}
			p.duel.gOpp, p.duel.gCenter, p.duel.gJoined, p.duel.gActive = x.gd.oppGuild, center, true, true
			p.queueDuelEnd(pendingDuelEnd{ledger: true})
			x.gd.count++
		}
	}
	enroll(g, mine)
	enroll(rg, theirs)
	r.Send(duelResult(0x2502, 1, me.CharacterID), "0x2502 guild duel accepted")
	start := func(opp uint32) []byte {
		return duelPacket(0x2503, func(w *packet.Writer) { w.F32(center[0]); w.F32(center[1]); w.U32(opp) })
	}
	sendPvPLocked(mine, start(rg.ID), "0x2503 guild duel countdown", false)
	sendPvPLocked(theirs, start(g.ID), "0x2503 guild duel countdown", false)
}

// tickGuildDuel is PvPChkLeaveByTime's guild part, run by the leader.
func (s *Session) tickGuildDuel() {
	me := s.player
	if me == nil {
		return
	}
	g, _ := guildAndRank(me)
	if g == nil {
		return
	}
	duelMu.Lock()
	gd := g.gd
	duelMu.Unlock()
	if gd.state == gduelNone || gd.leader != me.CharacterID || gd.deadline.IsZero() || time.Now().Before(gd.deadline) {
		return
	}
	og := guildFromID(gd.oppGuild)
	if og == nil {
		return
	}
	switch gd.state {
	case gduelAsking, gduelAsked:
		guildDuelCancel(g, og)
	case gduelCountdown:
		mine, theirs := guildPlayers(g), guildPlayers(og)
		duelMu.Lock()
		for _, x := range []*guild{g, og} {
			x.gd.state, x.gd.deadline = gduelFighting, time.Time{}
		}
		start := duelPacket(0x2504, nil)
		sendPvPLocked(mine, start, "0x2504 guild duel start", false)
		sendPvPLocked(theirs, start, "0x2504 guild duel start", false)
		duelMu.Unlock()
	}
}

// guildDuelCancel ends a guild duel before the countdown (0x2502 3 to
// both leaders).
func guildDuelCancel(g, og *guild) {
	duelMu.Lock()
	defer duelMu.Unlock()
	a, b := FindPlayerByID(g.gd.leader), FindPlayerByID(og.gd.leader)
	id := g.gd.leader
	if g.gd.state == gduelAsking {
		id = og.gd.leader
	}
	frame := duelResult(0x2502, 3, id)
	for _, p := range []*ConnectedPlayer{a, b} {
		if p != nil {
			p.duel.gOpp, p.duel.gJoined, p.duel.gActive = 0, false, false
			p.queueDuelEnd(pendingDuelEnd{ledger: true, debuffs: true})
			p.Send(frame, "0x2502 guild duel cancelled")
		}
	}
	g.gd, og.gd = guildDuel{}, guildDuel{}
}

// guildDuelLeave is the guild part of PvPChkLeaveByMove / ByDeath /
// ByZoneLeave / ByGuildLeave for p (g is p's guild, given when p has
// just left it).
func guildDuelLeave(p *ConnectedPlayer, g *guild, reason uint8) {
	if p == nil {
		return
	}
	if g == nil {
		g, _ = guildAndRank(p)
	}
	if g == nil {
		return
	}
	duelMu.Lock()
	gd, opp := g.gd, p.duel.gOpp
	duelMu.Unlock()
	if opp == 0 || gd.state == gduelNone {
		return
	}
	og := guildFromID(gd.oppGuild)
	if og == nil {
		return
	}
	switch gd.state {
	case gduelAsking, gduelAsked:
		if p.CharacterID == gd.leader {
			guildDuelCancel(g, og)
		}
	case gduelCountdown, gduelFighting:
		pvpUserLeave(g, og, p, reason)
	}
}

// pvpUserLeave is CGuild::PvPUserLeave.
func pvpUserLeave(g, og *guild, p *ConnectedPlayer, reason uint8) {
	mine, theirs := guildPlayers(g), guildPlayers(og)
	duelMu.Lock()
	defer duelMu.Unlock()
	if p.duel.gOpp == 0 {
		return
	}
	g.gd.count--
	leaderLeft := p.CharacterID == g.gd.leader && reason == 1
	note := duelPacket(0x2506, func(w *packet.Writer) { w.U8(reason); w.U32(p.CharacterID) })
	sendPvPLocked(theirs, note, "0x2506 guild duel member out", false)
	sendPvPLocked(mine, note, "0x2506 guild duel member out", false)
	if g.gd.count > 0 && !leaderLeft {
		p.duel.gOpp, p.duel.gActive = 0, false
		p.queueDuelEnd(pendingDuelEnd{ledger: true, debuffs: true, stopAttack: true})
		return
	}
	clear := func(players []*ConnectedPlayer, result uint8) {
		frame := duelByte(0x2505, result)
		for _, x := range players {
			if x.duel.gJoined {
				x.Send(frame, "0x2505 guild duel result")
			}
			if x.duel.gJoined || x.duel.gOpp != 0 {
				x.duel.gOpp, x.duel.gJoined, x.duel.gActive = 0, false, false
				x.queueDuelEnd(pendingDuelEnd{ledger: true, debuffs: true, stopAttack: true})
			}
		}
	}
	clear(theirs, 1)
	clear(mine, 2)
	p.duel.gOpp, p.duel.gJoined, p.duel.gActive = 0, false, false
	g.gd, og.gd = guildDuel{}, guildDuel{}
}

// guildDuelDeath is PvPGetDeathType 5: killed by a participant of the
// opposing guild; the leave runs and the death keeps the exp.
func (s *Session) guildDuelDeath(killer *ConnectedPlayer) bool {
	me := s.player
	if me == nil || killer == nil || !guildDuelFoes(me, killer) {
		return false
	}
	s.duelDeathMark = true
	guildDuelLeave(me, nil, 3)
	s.log.Printf("Rapture World guild duel death character_id=%d killer_id=%d", s.char.ID, killer.CharacterID)
	return true
}

// guildDuelCheckMove is PvPChkLeaveByMove's guild part.
func (s *Session) guildDuelCheckMove() {
	me := s.player
	duelMu.Lock()
	opp, active, center := me.duel.gOpp, me.duel.gActive, me.duel.gCenter
	duelMu.Unlock()
	if opp == 0 || !active || DistanceXZ(center[0], center[1], s.char.PosX, s.char.PosZ) <= duelRange {
		return
	}
	guildDuelLeave(me, nil, 1)
}
