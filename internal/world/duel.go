package world

import (
	"sync"
	"time"

	"github.com/Spelunkern/Rapture/internal/db"
	"github.com/Spelunkern/Rapture/internal/packet"
)

// Duels follow CUser::PacketPVP 0x0047d0f0 and the PvP* routines:
//
//   - 0x2401 request (target id): the requester and the target are free
//     (no duel, no guild duel), on the zone, of the same country (any
//     country on maps 39 and 40); else 0x2402 result 2; a debuff on either
//     (PvPHaveDebuff) answers 0x2402 result 4. Otherwise both get 0x2401
//     (requester, target), the requester is state 1 and the target state 2
//     for 10 s (then 0x2402 result 3 to both, PvPChkLeaveByTime).
//   - 0x2402 answer (u8): 0 refuses (0x2402 result 0 to the requester); else
//     both go to state 5, the requester gets 0x2402 result 1 and both open
//     the wager window with 0x2407 (opponent, 0).
//   - The window (CExchangePvP): 0x240A adds bag/slot/count to one of 8
//     wager slots (not quest items, count up to the stack, no duplicates;
//     0x240A echo, 0x240D the item to the opponent), 0x240B removes one
//     (0x240B 1/2), 0x240C sets the gold (0x240C 1/2); every change cancels
//     the ready marks (0x2409 3, 1 to both). 0x2409 0 marks ready (0x2409
//     1/2, 0); when both are, ExchangePvP takes each wager out of its
//     owner's inventory (0x0213, 0x0206, 0x2408 0), the center is the
//     midpoint, state 3 for 5 s with 0x2403 (center x, z), then state 4
//     and 0x2404 (damage ledgers reset). 0x2409 1 cancels ready, 2 closes
//     the window (0x2408 2 / 3, both end).
//   - State 4: the two are hostile (IsTeam) and not team-usable for
//     anyone. 0x2406 gives up (0x2406 2 to both); leaving the 40-unit
//     circle around the center 0x2406 1; another attacker or a mob hitting
//     either, or a death that is not the duel's, 0x2406 3; leaving the zone
//     0x2406 0 to the opponent. In all of these each wager drops back for
//     its owner (ItemDropPvP). In state 5 those events close the window
//     (0x2408 2 / 3) and in states 1-2 cancel the request (0x2402 3).
//   - A death whose top damager is the opponent (PvPGetDeathType 4) is
//     PvPChkLoseDeath: both wagers drop for the winner, victories + 1 and
//     defeats + 1 (up to 0xFFFF), 0x2405 1 to the winner and 2 to the
//     loser; no exp, kill or item drop, and the death keeps the exp, the
//     ultimate mode and the item buffs (m_nVehicleStatus -1).
//
// As in the original, the window's 40-unit check during state 5 measures
// from the last duel center (zero before the first duel), so moving with
// the window open closes it.

const (
	duelNone      = 0
	duelAsking    = 1
	duelAsked     = 2
	duelCountdown = 3
	duelFighting  = 4
	duelWindow    = 5

	duelRequestTime   = 10 * time.Second
	duelCountdownTime = 5 * time.Second
	duelRange         = 40
	duelWagerSlots    = 8
	duelCountCap      = 0xFFFF
)

type duelPick struct {
	Bag, Slot, Count uint8
	Item             db.Item
}

// duelState is a user's CExchangePvP and PvP fields (guarded by duelMu).
type duelState struct {
	state     int
	opponent  uint32
	deadline  time.Time
	center    [2]float32 // kept after the duel, as the original
	picks     [duelWagerSlots]*duelPick
	money     uint32
	ready     bool
	held      []db.Item
	heldMoney uint32
}

var duelMu sync.Mutex

func duelPacket(op packet.Opcode, write func(*packet.Writer)) []byte {
	w := packet.New(op)
	if write != nil {
		write(w)
	}
	return w.MustFinish()
}

func duelResult(op packet.Opcode, result uint8, id uint32) []byte {
	return duelPacket(op, func(w *packet.Writer) { w.U8(result); w.U32(id) })
}

func duelByte(op packet.Opcode, v uint8) []byte {
	return duelPacket(op, func(w *packet.Writer) { w.U8(v) })
}

// pendingDuelEnd asks a session to finish PvPModeEnd's own part.
type pendingDuelEnd struct{ ledger, debuffs, stopAttack bool }

func (p *ConnectedPlayer) queueDuelEnd(e pendingDuelEnd) {
	p.mailMu.Lock()
	p.duelEnds = append(p.duelEnds, e)
	p.mailMu.Unlock()
}

func (p *ConnectedPlayer) takeDuelEnds() []pendingDuelEnd {
	p.mailMu.Lock()
	defer p.mailMu.Unlock()
	e := p.duelEnds
	p.duelEnds = nil
	return e
}

// duelStateOf reads a user's duel state and opponent.
func duelStateOf(p *ConnectedPlayer) (int, uint32) {
	duelMu.Lock()
	defer duelMu.Unlock()
	return p.duel.state, p.duel.opponent
}

// duelFoes reports two users fighting each other (IsTeam's duel case).
func duelFoes(a, b *ConnectedPlayer) bool {
	duelMu.Lock()
	defer duelMu.Unlock()
	return a.duel.state == duelFighting && a.duel.opponent == b.CharacterID
}

// inActiveDuel is IsTeamUsable's state-4 check.
func inActiveDuel(p *ConnectedPlayer) bool {
	duelMu.Lock()
	defer duelMu.Unlock()
	return p.duel.state == duelFighting
}

// modeEnd is PvPModeEnd under duelMu (the debuffs and the ledger are
// cleared in the user's own session).
func (p *ConnectedPlayer) modeEndLocked() {
	p.duel.state, p.duel.opponent, p.duel.deadline = duelNone, 0, time.Time{}
	p.duel.picks, p.duel.money, p.duel.ready = [duelWagerSlots]*duelPick{}, 0, false
	p.queueDuelEnd(pendingDuelEnd{ledger: true, debuffs: true, stopAttack: true})
}

// dropWagerLocked is ItemDropPvP: src's held wager lies at owner's feet,
// owned by owner alone.
func dropWagerLocked(src, owner *ConnectedPlayer) {
	held, money := src.duel.held, src.duel.heldMoney
	src.duel.held, src.duel.heldMoney = nil, 0
	if owner == nil || (len(held) == 0 && money == 0) {
		return
	}
	mapID := owner.MapID()
	x, z := owner.PositionXZ()
	place := func(mi MapItem) {
		mi.ID, mi.OwnerID, mi.NoParty, mi.DroppedAt = nextMapItemID(), owner.CharacterID, true, time.Now()
		mi.PosX, mi.PosZ = dropPosition(x, z)
		addMapItem(mapID, mi)
		dispatchItemEnter(mapID, &mi, "0x0401 duel wager drop")
	}
	if money > 0 {
		place(MapItem{Item: db.Item{Type: moneyItemType, Count: 1}, Money: money})
	}
	for _, it := range held {
		place(MapItem{Item: it})
	}
}

// endDuelLocked ends a duel for both with each wager back to its owner;
// msg builds what each side receives (nil sends nothing).
func endDuelLocked(a, b *ConnectedPlayer, toA, toB []byte, label string) {
	if b != nil {
		dropWagerLocked(b, b)
		b.modeEndLocked()
		if toB != nil {
			b.Send(toB, label)
		}
	}
	dropWagerLocked(a, a)
	a.modeEndLocked()
	if toA != nil {
		a.Send(toA, label)
	}
}

// duelLeave is the shared shape of PvPChkLeaveByAttack / ByDeath /
// ByZoneLeave for p; fightCode is the 0x2406 result, toSelf whether p
// also gets it.
func duelLeave(p *ConnectedPlayer, fightCode uint8, toSelf bool) {
	duelMu.Lock()
	defer duelMu.Unlock()
	st := p.duel.state
	if st == duelNone {
		return
	}
	o := FindPlayerByID(p.duel.opponent)
	if o == nil {
		return
	}
	switch st {
	case duelAsking, duelAsked:
		id := p.CharacterID
		if st == duelAsking {
			id = o.CharacterID
		}
		frame := duelResult(0x2402, 3, id)
		o.modeEndLocked()
		p.modeEndLocked()
		o.Send(frame, "0x2402 duel request cancelled")
		p.Send(frame, "0x2402 duel request cancelled")
	case duelCountdown, duelFighting:
		frame := duelResult(0x2406, fightCode, p.CharacterID)
		var self []byte
		if toSelf {
			self = frame
		}
		endDuelLocked(p, o, self, frame, "0x2406 duel ended")
	case duelWindow:
		o.modeEndLocked()
		p.modeEndLocked()
		p.Send(duelByte(0x2408, 2), "0x2408 duel window closed")
		o.Send(duelByte(0x2408, 3), "0x2408 duel window closed")
	}
}

// duelHitBy is PvPChkLeaveByAttack when attacker (0 for a mob) hits p.
func duelHitBy(p *ConnectedPlayer, attacker uint32) {
	if p == nil {
		return
	}
	st, opp := duelStateOf(p)
	if st == duelNone || (attacker != 0 && opp == attacker) {
		return
	}
	duelLeave(p, 3, true)
}

// handleDuel dispatches 0x2401-0x240C.
func (s *Session) handleDuel(op packet.Opcode, p []byte) {
	me := s.player
	if me == nil || s.char == nil {
		return
	}
	switch op {
	case 0x2401:
		if len(p) >= 6 {
			id, _ := packet.U32(p[2:])
			s.duelRequest(id)
		}
	case 0x2402:
		if len(p) >= 3 {
			s.duelAnswer(p[2] != 0)
		}
	case 0x2406:
		s.duelGiveUp()
	case 0x2409:
		if len(p) >= 3 {
			s.duelReady(p[2])
		}
	case 0x240A:
		if len(p) >= 6 {
			s.duelAddItem(p[2], p[3], p[4], p[5])
		}
	case 0x240B:
		if len(p) >= 3 {
			s.duelRemoveItem(p[2])
		}
	case 0x240C:
		if len(p) >= 6 {
			m, _ := packet.U32(p[2:])
			s.duelMoney(m)
		}
	}
}

func (s *Session) duelRequest(target uint32) {
	me, ch := s.player, s.char
	t := FindPlayerByID(target)
	duelMu.Lock()
	defer duelMu.Unlock()
	if me.duel.state != duelNone || me.guildDuelState() != 0 {
		return
	}
	mapOK := realMap(ch.MapID) == 39 || realMap(ch.MapID) == 40
	if t == nil || t == me || t.MapID() != ch.MapID || t.duel.state != duelNone || t.guildDuelState() != 0 ||
		(t.Faction() != s.faction && !mapOK) {
		s.send(duelResult(0x2402, 2, target), "0x2402 duel refused")
		return
	}
	if t.debuffed.Load() || me.debuffed.Load() {
		s.send(duelResult(0x2402, 4, target), "0x2402 duel refused (debuff)")
		return
	}
	deadline := time.Now().Add(duelRequestTime)
	me.duel.state, me.duel.opponent, me.duel.deadline = duelAsking, target, deadline
	t.duel.state, t.duel.opponent, t.duel.deadline = duelAsked, ch.ID, deadline
	frame := duelPacket(0x2401, func(w *packet.Writer) { w.U32(ch.ID); w.U32(target) })
	s.send(frame, "0x2401 duel request")
	t.Send(frame, "0x2401 duel request")
}

func (s *Session) duelAnswer(accept bool) {
	me := s.player
	duelMu.Lock()
	defer duelMu.Unlock()
	if me.duel.state != duelAsked {
		return
	}
	r := FindPlayerByID(me.duel.opponent)
	if r == nil {
		return
	}
	if !accept {
		r.duel.state, r.duel.opponent, r.duel.deadline = duelNone, 0, time.Time{}
		me.duel.state, me.duel.opponent, me.duel.deadline = duelNone, 0, time.Time{}
		r.Send(duelResult(0x2402, 0, me.CharacterID), "0x2402 duel refused")
		return
	}
	for _, p := range []*ConnectedPlayer{me, r} {
		p.duel.state, p.duel.deadline = duelWindow, time.Time{}
		p.duel.picks, p.duel.money, p.duel.ready = [duelWagerSlots]*duelPick{}, 0, false
	}
	r.Send(duelResult(0x2402, 1, me.CharacterID), "0x2402 duel accepted")
	open := func(peer uint32) []byte {
		return duelPacket(0x2407, func(w *packet.Writer) { w.U32(peer); w.U8(0) })
	}
	me.Send(open(r.CharacterID), "0x2407 duel window")
	r.Send(open(me.CharacterID), "0x2407 duel window")
}

func (s *Session) duelGiveUp() {
	me := s.player
	duelMu.Lock()
	defer duelMu.Unlock()
	if me.duel.state != duelFighting {
		return
	}
	frame := duelResult(0x2406, 2, me.CharacterID)
	endDuelLocked(me, FindPlayerByID(me.duel.opponent), frame, frame, "0x2406 duel given up")
}

// windowPeerLocked returns the opponent while both are in the window.
func (s *Session) windowPeerLocked() *ConnectedPlayer {
	me := s.player
	if me.duel.state != duelWindow {
		return nil
	}
	o := FindPlayerByID(me.duel.opponent)
	if o == nil || o.duel.state != duelWindow {
		return nil
	}
	return o
}

// cancelReadyLocked is ExchangeCancleReadyPvP.
func cancelReadyLocked(a, b *ConnectedPlayer) {
	if !a.duel.ready && !b.duel.ready {
		return
	}
	a.duel.ready, b.duel.ready = false, false
	frame := duelPacket(0x2409, func(w *packet.Writer) { w.U8(3); w.U8(1) })
	a.Send(frame, "0x2409 duel ready cancelled")
	b.Send(frame, "0x2409 duel ready cancelled")
}

func (s *Session) duelReady(action uint8) {
	me := s.player
	duelMu.Lock()
	defer duelMu.Unlock()
	o := s.windowPeerLocked()
	if o == nil {
		return
	}
	switch action {
	case 0:
		me.duel.ready = true
		s.send(duelPacket(0x2409, func(w *packet.Writer) { w.U8(1); w.U8(0) }), "0x2409 duel ready")
		o.Send(duelPacket(0x2409, func(w *packet.Writer) { w.U8(2); w.U8(0) }), "0x2409 duel ready")
		if !o.duel.ready {
			return
		}
		s.exchangeWagerSelf()
		exchangeWagerOther(o, s.log)
		ox, oz := o.PositionXZ()
		center := [2]float32{(s.char.PosX + ox) / 2, (s.char.PosZ + oz) / 2}
		deadline := time.Now().Add(duelCountdownTime)
		for _, p := range []*ConnectedPlayer{me, o} {
			p.duel.state, p.duel.center, p.duel.deadline = duelCountdown, center, deadline
		}
		frame := duelPacket(0x2403, func(w *packet.Writer) { w.F32(center[0]); w.F32(center[1]) })
		me.Send(frame, "0x2403 duel countdown")
		o.Send(frame, "0x2403 duel countdown")
	case 1:
		cancelReadyLocked(me, o)
	case 2:
		s.send(duelByte(0x2408, 2), "0x2408 duel window closed")
		o.Send(duelByte(0x2408, 3), "0x2408 duel window closed")
		o.modeEndLocked()
		me.modeEndLocked()
	}
}

func (s *Session) duelAddItem(bag, slot, count, wslot uint8) {
	me := s.player
	duelMu.Lock()
	defer duelMu.Unlock()
	o := s.windowPeerLocked()
	if o == nil || count == 0 || wslot >= duelWagerSlots || me.duel.picks[wslot] != nil ||
		bag < InventoryFirstBag || bag > InventoryLastBag || slot >= InventorySlotCount {
		return
	}
	i := findItem(s.items, bag, slot)
	if i < 0 {
		return
	}
	it := s.items[i]
	if realType(it.Type) == 27 || it.Count < count {
		return
	}
	for _, pk := range me.duel.picks {
		if pk != nil && pk.Bag == bag && pk.Slot == slot {
			return
		}
	}
	cancelReadyLocked(me, o)
	me.duel.picks[wslot] = &duelPick{Bag: bag, Slot: slot, Count: count, Item: it}
	s.send(duelPacket(0x240A, func(w *packet.Writer) { w.U8(bag); w.U8(slot); w.U8(count); w.U8(wslot) }), "0x240A duel wager item")
	o.Send(duelPacket(0x240D, func(w *packet.Writer) {
		w.U8(wslot)
		writeOffered(w, &offeredItem{Item: it, Count: count})
	}), "0x240D duel wager item (opponent)")
}

func (s *Session) duelRemoveItem(wslot uint8) {
	me := s.player
	duelMu.Lock()
	defer duelMu.Unlock()
	o := s.windowPeerLocked()
	if o == nil || wslot >= duelWagerSlots || me.duel.picks[wslot] == nil {
		return
	}
	cancelReadyLocked(me, o)
	me.duel.picks[wslot] = nil
	s.send(duelPacket(0x240B, func(w *packet.Writer) { w.U8(1); w.U8(wslot) }), "0x240B duel wager removed")
	o.Send(duelPacket(0x240B, func(w *packet.Writer) { w.U8(2); w.U8(wslot) }), "0x240B duel wager removed")
}

func (s *Session) duelMoney(m uint32) {
	me := s.player
	duelMu.Lock()
	defer duelMu.Unlock()
	o := s.windowPeerLocked()
	if o == nil || m > s.char.Money {
		return
	}
	cancelReadyLocked(me, o)
	me.duel.money = m
	s.send(duelPacket(0x240C, func(w *packet.Writer) { w.U8(1); w.U32(m) }), "0x240C duel wager gold")
	o.Send(duelPacket(0x240C, func(w *packet.Writer) { w.U8(2); w.U32(m) }), "0x240C duel wager gold")
}

// takeWager is ExchangePvP over one inventory: the gold (up to what the
// owner has) and each picked item move into the held wager; a partial
// stack becomes a new item without quality or gems. It returns the 0x0206
// packets.
func takeWager(charID uint32, d *duelState, items *[]db.Item, money uint32) ([][]byte, uint32) {
	var out [][]byte
	gold := min(d.money, money)
	d.heldMoney = gold
	for _, pk := range d.picks {
		if pk == nil {
			continue
		}
		i := findItem(*items, pk.Bag, pk.Slot)
		if i < 0 {
			continue
		}
		it := &(*items)[i]
		def := db.MustDefs().Item(it.Type, it.TypeID)
		whole := (def != nil && def.Count == 1 && it.Count == 1) || pk.Count == it.Count
		if whole {
			held := *it
			held.UID, held.Bag, held.Slot = 0, 0, 0
			d.held = append(d.held, held)
			_ = db.DeleteCharacterItems(charID, it.UID)
			out = append(out, buildRemoveItem(it, true))
			*items = append((*items)[:i], (*items)[i+1:]...)
			continue
		}
		if pk.Count > it.Count {
			continue
		}
		it.Count -= pk.Count
		_ = db.SaveCharacterItem(charID, it)
		held := db.Item{Type: it.Type, TypeID: it.TypeID, Count: pk.Count, MakeTime: time.Now(), MakeType: "T"}
		d.held = append(d.held, held)
		out = append(out, buildRemoveItem(it, false))
	}
	d.picks = [duelWagerSlots]*duelPick{}
	return out, gold
}

// exchangeWagerSelf runs ExchangePvP on this session's own inventory.
func (s *Session) exchangeWagerSelf() {
	me := s.player
	pk, gold := takeWager(s.char.ID, &me.duel, &s.items, s.char.Money)
	if gold > 0 {
		s.char.Money -= gold
		s.persist()
		s.send(buildSetMoney(s.char.Money), "0x0213 duel wager gold")
	}
	for _, p := range pk {
		s.send(p, "0x0206 duel wager item")
	}
	me.UpdateItems(s.items)
	s.send(duelByte(0x2408, 0), "0x2408 duel window done")
}

// exchangeWagerOther runs ExchangePvP for the opponent through the
// database; its session reloads with the trade-refresh mailbox.
func exchangeWagerOther(o *ConnectedPlayer, log logPrinter) {
	items, err := db.CharacterItems(o.CharacterID)
	if err != nil {
		log.Printf("Rapture World duel wager load failed: %v", err)
		items = nil
	}
	pk, gold := takeWager(o.CharacterID, &o.duel, &items, o.Snapshot().Money)
	o.QueueTradeRefresh(pk, -int64(gold))
	o.Send(duelByte(0x2408, 0), "0x2408 duel window done")
}

// tickDuel is PvPChkLeaveByTime and the session's part of PvPModeEnd.
func (s *Session) tickDuel() {
	me := s.player
	if me == nil || s.char == nil {
		return
	}
	debuffed := false
	for _, b := range s.buffs {
		if b.Effect == typeEffectDebuff {
			debuffed = true
		}
	}
	me.debuffed.Store(debuffed)
	for _, e := range me.takeDuelEnds() {
		if e.debuffs {
			s.removeEffects(func(b *ActiveBuff) bool { return b.Effect == typeEffectDebuff }, "(duel end)")
		}
		if e.ledger {
			s.damageLedger = nil
		}
		if e.stopAttack && s.autoAttackUser {
			s.autoAttack = false
		}
	}
	duelMu.Lock()
	defer duelMu.Unlock()
	d := &me.duel
	if d.state == duelNone || d.deadline.IsZero() || time.Now().Before(d.deadline) {
		return
	}
	o := FindPlayerByID(d.opponent)
	if o == nil {
		return
	}
	switch d.state {
	case duelAsking, duelAsked:
		id := me.CharacterID
		if d.state == duelAsking {
			id = o.CharacterID
		}
		frame := duelResult(0x2402, 3, id)
		o.modeEndLocked()
		me.modeEndLocked()
		o.Send(frame, "0x2402 duel request expired")
		me.Send(frame, "0x2402 duel request expired")
	case duelCountdown:
		for _, p := range []*ConnectedPlayer{me, o} {
			p.duel.state, p.duel.deadline = duelFighting, time.Time{}
			p.queueDuelEnd(pendingDuelEnd{ledger: true})
			p.Send(duelPacket(0x2404, nil), "0x2404 duel start")
		}
	}
}

// duelCheckMove is PvPChkLeaveByMove after this user moved.
func (s *Session) duelCheckMove() {
	me := s.player
	if me == nil {
		return
	}
	duelMu.Lock()
	d := me.duel
	duelMu.Unlock()
	if DistanceXZ(d.center[0], d.center[1], s.char.PosX, s.char.PosZ) <= duelRange {
		return
	}
	switch d.state {
	case duelCountdown, duelFighting:
		duelMu.Lock()
		frame := duelResult(0x2406, 1, me.CharacterID)
		endDuelLocked(me, FindPlayerByID(me.duel.opponent), frame, frame, "0x2406 duel left")
		duelMu.Unlock()
	case duelWindow:
		duelMu.Lock()
		if o := FindPlayerByID(me.duel.opponent); o != nil {
			o.modeEndLocked()
			o.Send(duelByte(0x2408, 3), "0x2408 duel window closed (moved)")
		}
		me.modeEndLocked()
		duelMu.Unlock()
		s.send(duelByte(0x2408, 2), "0x2408 duel window closed (moved)")
	}
}

// duelDeath is PvPGetDeathType 4 + PvPChkLoseDeath for this (dead) user
// killed by winner; false when the death is not the duel's.
func (s *Session) duelDeath(winner *ConnectedPlayer) bool {
	me := s.player
	if me == nil || winner == nil {
		return false
	}
	duelMu.Lock()
	defer duelMu.Unlock()
	if me.duel.state != duelFighting || me.duel.opponent != winner.CharacterID {
		return false
	}
	dropWagerLocked(winner, winner)
	dropWagerLocked(me, winner)
	for _, p := range []*ConnectedPlayer{me, winner} {
		p.duel.state, p.duel.opponent, p.duel.deadline = duelNone, 0, time.Time{}
		p.queueDuelEnd(pendingDuelEnd{ledger: true, debuffs: true, stopAttack: true})
	}
	if s.char.Defeats < duelCountCap {
		s.char.Defeats++
	}
	s.persist()
	winner.queueDuelWin()
	winner.Send(duelByte(0x2405, 1), "0x2405 duel won")
	s.send(duelByte(0x2405, 2), "0x2405 duel lost")
	s.duelDeathMark = true
	s.log.Printf("Rapture World duel lost character_id=%d winner_id=%d", s.char.ID, winner.CharacterID)
	return true
}

func (p *ConnectedPlayer) queueDuelWin() {
	p.mailMu.Lock()
	p.duelWins++
	p.mailMu.Unlock()
}

func (p *ConnectedPlayer) takeDuelWins() int {
	p.mailMu.Lock()
	defer p.mailMu.Unlock()
	n := p.duelWins
	p.duelWins = 0
	return n
}

// applyDuelWins books victories in the winner's session.
func (s *Session) applyDuelWins() {
	if s.player == nil || s.char == nil {
		return
	}
	n := s.player.takeDuelWins()
	if n == 0 {
		return
	}
	for ; n > 0; n-- {
		if s.char.Victories < duelCountCap {
			s.char.Victories++
		}
	}
	s.persist()
}

// guildDuelState is the guild duel state (0 without one; guild duels are
// in guildduel.go).
func (p *ConnectedPlayer) guildDuelState() int { return 0 }
