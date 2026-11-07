package world

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/Spelunkern/Rapture/internal/db"
	"github.com/Spelunkern/Rapture/internal/packet"
)

// Death by a user follows CUser::Death 0x004586a0:
//
//   - CDamage (Add 0x004b8b60 from CUser::Damage, GetMax 0x004b8c40, Reset
//     at every death) keeps the HP damage taken per attacker; the reward
//     goes to the attacker with the most damage (the first one on a tie),
//     not to the last hit. A mob or the user itself at the top gives
//     nothing. With no entry at all, a type-1 death still runs
//     ItemDropByUser for the reported killer.
//   - Normal PvP (PvPGetDeathType 3; duels are 4 and 5 and do not exist
//     yet): a killer without a party gets CUser::GetExp 0x004581b0 x 1.2
//     through AddExpFromUser (exp charms apply), KillPCFrom, then
//     ItemDropByUser. In a party, every member within 100 units gets
//     GetExp(own level) x m_PartyExp[size] (m_RaidPartyExp for a raid) x
//     GetPartyExpByLevel, the first of them KillPCFrom and the others
//     KillPCTo.
//   - KillPCFrom 0x0045b050: KillPCTo for the killer, then the dead user's
//     deaths + 1 (up to g_tDeathInfo[15] 200000) with 0x020E index 1.
//     KillPCTo 0x0045aed0: QuestUpdateKillPC, kills + 1 (up to
//     g_tKillInfo[15] 200000; the NEW_SERVER doubling is off in the EP4.5
//     ps_game.ini), 0x020E index 0, 0x0522 (id, kills) to the view.
//   - ItemDropByUser 0x0045a180, above level 10 and without ability 37
//     (SetSkillAbility index 36, m_bSkillNDItemEQ): a regular account
//     drops one item at a chance set by its Vg (|Vg| > 190: 65%, > 170: 70,
//     > 140: 80, > 100: 85, > 50: 90, > 10: 95, else 100) among the first
//     10 inventory items with RealType 1-25, ReqOg 0 and Type not 94; a GM
//     account (permission 1-10) drops 1-3 items among the first 25 with
//     Type not 94. Then, 30% of the time, gold: 1..level x 5 (up to all of
//     it), split among the killer's party within 100 units or dropped for
//     the killer.
//   - ItemDropByUserDeath 0x0045a5f0 places the item by the killer party's
//     item division: 2 a random member gets it directly when in range
//     (otherwise it is lost, as in the original), 1 the next member in
//     range in turn gets it directly, 3 the next member in range owns it
//     on the ground; else (or nobody in range) it lies on the ground for
//     the killer and the killer's party.
//
// Not modeled: CWorld::AddKillCount (the country kill tally behind the
// KC status), duel and guild duel deaths.

const (
	damageByUser       = 1
	damageByMob        = 2
	pvpKillCap         = 200000
	pvpDeathCap        = 200000
	pvpExpFactor       = 1.2
	pvpDropLevel       = 10
	pvpNoDropAbility   = 37
	pvpUndroppableType = 94
	pvpGoldChance      = 30
)

// damageEntry is one CDamage node.
type damageEntry struct {
	Type   uint8
	ID     uint32
	Amount uint32
}

// addDamage is CDamage::Add.
func (s *Session) addDamage(kind uint8, id, amount uint32) {
	for i := range s.damageLedger {
		if e := &s.damageLedger[i]; e.Type == kind && e.ID == id {
			e.Amount += amount
			return
		}
	}
	s.damageLedger = append(s.damageLedger, damageEntry{kind, id, amount})
}

// topDamage is CDamage::GetMax: the first entry with the highest damage.
func (s *Session) topDamage() (damageEntry, bool) {
	if len(s.damageLedger) == 0 {
		return damageEntry{}, false
	}
	top := s.damageLedger[0]
	for _, e := range s.damageLedger[1:] {
		if top.Amount < e.Amount {
			top = e
		}
	}
	return top, true
}

// pvpExp is CUser::GetExp: the dead user's level x 50 x ((dead - killer +
// 1) x 0.2 + 1), at least 1, and 0 when the dead user is 10+ levels above.
func pvpExp(deadLevel, killerLevel uint16) uint32 {
	diff := int32(deadLevel) - int32(killerLevel)
	if diff > 9 {
		return 0
	}
	v := int64(math.Trunc(float64(int32(deadLevel)*50) * (float64(diff+1)*0.2 + 1)))
	return uint32(max(v, 1))
}

// pvpKill is a kill credited to a user's session (KillPCTo).
type pvpKill struct{ VictimID uint32 }

func (p *ConnectedPlayer) QueuePvPKill(k pvpKill) {
	p.mailMu.Lock()
	p.pvpKills = append(p.pvpKills, k)
	p.mailMu.Unlock()
}

func (p *ConnectedPlayer) TakePvPKills() []pvpKill {
	p.mailMu.Lock()
	defer p.mailMu.Unlock()
	k := p.pvpKills
	p.pvpKills = nil
	return k
}

// QueueItemGrant hands an item straight to a user (ItemGet).
func (p *ConnectedPlayer) QueueItemGrant(it db.Item) {
	p.mailMu.Lock()
	p.itemGrants = append(p.itemGrants, it)
	p.mailMu.Unlock()
}

func (p *ConnectedPlayer) TakeItemGrants() []db.Item {
	p.mailMu.Lock()
	defer p.mailMu.Unlock()
	g := p.itemGrants
	p.itemGrants = nil
	return g
}

// tickPvP applies the kills and items other sessions credited to this one.
func (s *Session) tickPvP() {
	if s.player == nil || s.char == nil {
		return
	}
	for range s.player.TakePvPKills() {
		s.killPCTo()
	}
	for _, it := range s.player.TakeItemGrants() {
		adds, left, err := s.giveItem(&it, it.Count, maxStackOf(it.Type, it.TypeID))
		if err != nil {
			s.log.Printf("Rapture World pvp item grant failed: %v", err)
		}
		for _, a := range adds {
			s.send(a, "0x0205 pvp item")
		}
		if left > 0 {
			// No room: the item lands at the user's feet, theirs alone.
			it.Count = left
			s.placeGroundItem(it, s.char.ID, true)
		}
		if s.player != nil {
			s.player.UpdateItems(s.items)
		}
	}
}

// killPCTo is CUser::KillPCTo on this (the killing) user.
func (s *Session) killPCTo() {
	ch := s.char
	s.questKillPC()
	if ch.Kills >= pvpKillCap {
		return
	}
	ch.Kills++
	s.persist()
	s.send(buildKillCountUpdate(0, ch.Kills), "0x020E kills")
	w := packet.New(0x0522)
	w.U32(ch.ID)
	w.U32(ch.Kills)
	frame := w.MustFinish()
	s.send(frame, "0x0522 kill count")
	s.sendToViewers(frame, "0x0522 kill count (view)")
}

// questKillPC is CUser::QuestUpdateKillPC: the first active quest that
// ends on player kills counts one.
func (s *Session) questKillPC() {
	for _, q := range s.quests {
		if q.def.EndType != 3 {
			continue
		}
		if q.counters[0] == 255 {
			return
		}
		s.bumpQuestCounter(q, 0)
		if q.def.EndNpcType == 0 && q.def.PvpKills <= q.counters[0] {
			s.completeQuest(q, 0, true)
		}
		return
	}
}

// deathByUser runs CUser::Death's reward part on this (the dead) user.
func (s *Session) deathByUser(reported uint32) {
	ch := s.char
	defer func() { s.damageLedger = nil }() // CDamage::Reset
	top, ok := s.topDamage()
	if !ok {
		if reported != ch.ID {
			s.itemDropByUser(FindPlayerByID(reported))
		}
		return
	}
	if top.Type != damageByUser || top.ID == ch.ID {
		return
	}
	killer := FindPlayerByID(top.ID)
	if killer == nil {
		return
	}
	partyMu.Lock()
	pt := partyOf(killer)
	var members []*ConnectedPlayer
	var maxLevel uint16
	raid := false
	if pt != nil {
		members, maxLevel, raid = append(members, pt.members...), pt.maxLevel, pt.isRaid()
	}
	partyMu.Unlock()
	if len(members) == 0 {
		exp := uint32(math.Trunc(float64(pvpExp(ch.Level, killer.Snapshot().Level)) * pvpExpFactor))
		if exp > 0 {
			killer.QueueExp(exp)
		}
		s.killPCFrom(killer)
		s.itemDropByUser(killer)
		return
	}
	factor := partyExpFactor[min(len(members), len(partyExpFactor)-1)]
	if raid {
		factor = raidExpFactor[min(len(members), len(raidExpFactor)-1)]
	}
	var near []*ConnectedPlayer
	for _, m := range members {
		mx, mz := m.PositionXZ()
		if m.MapID() != ch.MapID || DistanceXZ(mx, mz, ch.PosX, ch.PosZ) > partyRewardRange {
			continue
		}
		lvl := m.Snapshot().Level
		exp := int64(math.Trunc(float64(pvpExp(ch.Level, lvl)) * factor))
		exp = int64(math.Trunc(partyExpLevelShare(maxLevel, lvl) * float64(exp)))
		if exp > 0 {
			m.QueueExp(uint32(exp))
		}
		near = append(near, m)
	}
	for i, m := range near {
		if i == 0 {
			s.killPCFrom(m)
		} else {
			m.QueuePvPKill(pvpKill{VictimID: ch.ID})
		}
	}
	s.itemDropByUser(killer)
}

// killPCFrom is CUser::KillPCFrom: the killer's KillPCTo, then this
// user's death count.
func (s *Session) killPCFrom(killer *ConnectedPlayer) {
	ch := s.char
	killer.QueuePvPKill(pvpKill{VictimID: ch.ID})
	if ch.Deaths >= pvpDeathCap {
		return
	}
	ch.Deaths++
	s.persist()
	s.send(buildKillCountUpdate(1, ch.Deaths), "0x020E deaths")
}

// itemDropByUser is CUser::ItemDropByUser on this (the dead) user.
func (s *Session) itemDropByUser(killer *ConnectedPlayer) {
	ch := s.char
	if ch.Level <= pvpDropLevel || s.hasBenefit(pvpNoDropAbility) {
		return
	}
	defs := db.MustDefs()
	candidates := func(limit int, ok func(*db.ItemDef) bool) []db.Item {
		var out []db.Item
		for bag := uint8(InventoryFirstBag); bag <= InventoryLastBag && len(out) < limit; bag++ {
			for slot := uint8(0); slot < InventorySlotCount && len(out) < limit; slot++ {
				i := findItem(s.items, bag, slot)
				if i < 0 {
					continue
				}
				if d := defs.Item(s.items[i].Type, s.items[i].TypeID); d != nil && d.Type != pvpUndroppableType && ok(d) {
					out = append(out, s.items[i])
				}
			}
		}
		return out
	}
	if s.isAdmin {
		for n := rand.IntN(3) + 1; n > 0; n-- {
			if c := candidates(25, func(*db.ItemDef) bool { return true }); len(c) > 0 {
				s.itemDropByUserDeath(c[rand.IntN(len(c))], killer)
			}
		}
	} else if rand.IntN(100) < pvpDropChance(ch.Vg) {
		c := candidates(10, func(d *db.ItemDef) bool {
			rt := realType(d.Type)
			return rt >= 1 && rt <= 25 && d.ReqOg == 0
		})
		if len(c) > 0 {
			s.itemDropByUserDeath(c[rand.IntN(len(c))], killer)
		}
	}
	if ch.Money == 0 || rand.IntN(100) >= pvpGoldChance {
		return
	}
	amount := min(rand.Uint32N(uint32(ch.Level)*5)+1, ch.Money)
	ch.Money -= amount
	s.persist()
	s.send(buildSetMoney(ch.Money), "0x0213 pvp gold lost")
	owner := uint32(0)
	if killer != nil {
		partyMu.Lock()
		pt := partyOf(killer)
		var members []*ConnectedPlayer
		if pt != nil {
			members = append(members, pt.members...)
		}
		partyMu.Unlock()
		if len(members) > 0 {
			share := max(uint32(float32(amount)/float32(len(members))), 1)
			for _, m := range members {
				mx, mz := m.PositionXZ()
				if m.MapID() == ch.MapID && DistanceXZ(mx, mz, ch.PosX, ch.PosZ) <= partyRewardRange {
					m.QueueGold(share)
				}
			}
			return
		}
		owner = killer.CharacterID
	}
	x, z := dropPosition(ch.PosX, ch.PosZ)
	mi := MapItem{ID: nextMapItemID(), OwnerID: owner, NoParty: true, DroppedAt: time.Now(), PosX: x, PosZ: z,
		Item: db.Item{Type: moneyItemType, Count: 1}, Money: amount}
	addMapItem(ch.MapID, mi)
	dispatchItemEnter(ch.MapID, &mi, "0x0401 pvp gold drop")
	s.log.Printf("Rapture World pvp gold drop character_id=%d amount=%d owner=%d", ch.ID, amount, owner)
}

// pvpDropChance is ItemDropByUser's Vg table.
func pvpDropChance(vg int16) int {
	v := int32(vg)
	switch {
	case v < -190 || v > 190:
		return 65
	case v < -170 || v > 170:
		return 70
	case v < -140 || v > 140:
		return 80
	case v < -100 || v > 100:
		return 85
	case v < -50 || v > 50:
		return 90
	case v < -10 || v > 10:
		return 95
	}
	return 100
}

// itemDropByUserDeath is CUser::ItemDropByUserDeath for one inventory item.
func (s *Session) itemDropByUserDeath(it db.Item, killer *ConnectedPlayer) {
	ch := s.char
	if err := db.DeleteCharacterItems(ch.ID, it.UID); err != nil {
		s.log.Printf("Rapture World pvp item drop failed: %v", err)
		return
	}
	if i := findItem(s.items, it.Bag, it.Slot); i >= 0 {
		s.items = append(s.items[:i], s.items[i+1:]...)
	}
	if s.player != nil {
		s.player.UpdateItems(s.items)
	}
	s.send(buildRemoveItem(&it, true), "0x0206 pvp item lost")
	s.log.Printf("Rapture World pvp item drop character_id=%d item=%d/%d bag=%d slot=%d",
		ch.ID, it.Type, it.TypeID, it.Bag, it.Slot)
	dropped := it
	dropped.UID, dropped.Bag, dropped.Slot = 0, 0, 0
	if killer == nil {
		s.placeGroundItem(dropped, 0, false)
		return
	}
	inRange := func(m *ConnectedPlayer) bool {
		mx, mz := m.PositionXZ()
		return m.MapID() == ch.MapID && DistanceXZ(mx, mz, ch.PosX, ch.PosZ) <= partyRewardRange
	}
	partyMu.Lock()
	pt := partyOf(killer)
	var members []*ConnectedPlayer
	division := int32(0)
	if pt != nil {
		members, division = append(members, pt.members...), pt.itemDivision
	}
	var pick *ConnectedPlayer
	switch {
	case pt == nil:
	case division == 2:
		if m := members[rand.IntN(len(members))]; inRange(m) {
			pick = m
		} else {
			partyMu.Unlock()
			s.log.Printf("Rapture World pvp item lost (random member out of range) item=%d/%d", it.Type, it.TypeID)
			return
		}
	case division == 1 || division == 3:
		for range members {
			if pt.itemDivSeq >= len(members) {
				pt.itemDivSeq = 0
			}
			m := members[pt.itemDivSeq]
			pt.itemDivSeq++
			if inRange(m) {
				pick = m
				break
			}
		}
	}
	partyMu.Unlock()
	switch {
	case pick != nil && division == 3:
		s.placeGroundItem(dropped, pick.CharacterID, true)
	case pick != nil:
		pick.QueueItemGrant(dropped)
	default:
		s.placeGroundItem(dropped, killer.CharacterID, false)
	}
}

// placeGroundItem drops an item at this user's feet (drop type 3).
func (s *Session) placeGroundItem(it db.Item, owner uint32, noParty bool) {
	ch := s.char
	x, z := dropPosition(ch.PosX, ch.PosZ)
	mi := MapItem{ID: nextMapItemID(), OwnerID: owner, NoParty: noParty, DroppedAt: time.Now(), PosX: x, PosZ: z, Item: it}
	addMapItem(ch.MapID, mi)
	dispatchItemEnter(ch.MapID, &mi, "0x0401 pvp item drop")
}
