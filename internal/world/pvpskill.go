package world

import (
	"time"

	"github.com/Spelunkern/Rapture/internal/db"
	"github.com/Spelunkern/Rapture/internal/packet"
)

// Hostile skills on users follow ps_game.exe:
//
//   - 0x0511 (a skill on a user) with TypeEffect 1 or 3: CUser::SkillAttack
//     0x00451d90 / SkillDebuff 0x00452bf0 pick TargetType 3 (one hostile
//     user, CUser::ChkTargetUser 0x0044e140: !IsTeam and attackable, else
//     result 11), 6 (around the caster, SkillAttackRange 0x00451f80 /
//     SkillDebuffRange 0x00452c50) or 7 (around the target user,
//     SkillAttackRangeSelect 0x00452560 / SkillDebuffRangeSelect
//     0x00453050); an area announces 0x0511 result 4 and hits the hostile
//     users within ApplyRange first, then the mobs.
//   - 0x0517 TargetType 7 (around a mob, 0x00450f70 / 0x00451570) hits the
//     hostile users around the mob too; 0x0517 TargetType 6 is mobs only.
//   - CUser::AttackUser(skill) 0x0044f4c0 on each user: a miss sends 0x0511
//     (0x0512 for a drain) or, for later and area hits, 0x0513, result 3;
//     a hit runs ChkMirror, AttackDamage (the weapon wears on a critical),
//     CUser::Damage, 0x0511 / 0x0512 (drain: the caster heals the damage)
//     / 0x0513 to the view, then the skill's AddDamage with 0x0514, a
//     mirrored hit back with 0x0515, and a debuff (TypeEffect 3) stays on
//     a target that survived. Area hits skip drains; detail 22 repeats
//     MultiAttack times; detail 23 then sacrifices the caster.
//
// Not modeled: skill 56's move-speed change, the HP-ability shields
// (abilities 7/8) and state resistances in AttackSuccess, the clone of
// skill 100.

const (
	opUserSkillDrain packet.Opcode = 0x0512
	opUserSkillOther packet.Opcode = 0x0513
	opUserSkillExtra packet.Opcode = 0x0514
	opUserSkillPvP   packet.Opcode = packet.SkillUseNoTarget // 0x0511
)

// userSkillCenter resolves a 0x0511 hostile skill's target user; ok false
// is ChkTargetUser's refusal.
func (s *Session) userSkillCenter(def *db.SkillDef, target uint32) (*ConnectedPlayer, bool) {
	if def.TargetType == targetTypeEnemiesNearCaster {
		return nil, true
	}
	p := s.hostileUser(target)
	return p, p != nil
}

// hostileUsersWithin lists the alive hostile users within r of (x, z).
func (s *Session) hostileUsersWithin(x, z, r float32) []*ConnectedPlayer {
	var out []*ConnectedPlayer
	for _, p := range PlayersOnMap(s.char.MapID) {
		if p == s.player || !p.IsAlive() || s.isTeam(p) {
			continue
		}
		px, pz := p.PositionXZ()
		if DistanceXZ(x, z, px, pz) <= r {
			out = append(out, p)
		}
	}
	return out
}

// userSkillAttack runs an attack or debuff skill sent as 0x0511.
func (s *Session) userSkillAttack(k db.CharacterSkill, def *db.SkillDef, target uint32, castPos *[2]float32, stats CharacterStats) {
	ch := s.char
	if def.TypeEffect == typeEffectAttack && (def.TypeDetail < detailAttack || def.TypeDetail > detailDrain) {
		s.log.Printf("Rapture World rejected skill use: attack detail %d not modeled skill_id=%d", def.TypeDetail, k.SkillID)
		return
	}
	if stats.Disabled[min(int(def.Kind), 2)] > 0 {
		return
	}
	center, ok := s.userSkillCenter(def, target)
	if !ok {
		s.send(buildSkillUseResult(opUserSkillPvP, skillResultNotTarget, ch.ID, 0, k.SkillID, k.Level, 0, 0, 0), "0x0511 skill target refused")
		return
	}
	if center != nil {
		x, z := center.PositionXZ()
		if castPos != nil {
			if DistanceXZ(castPos[0], castPos[1], x, z) > skillTargetEscapeRange {
				return
			}
		} else if !s.inUserSkillRange(center, def) {
			s.send(buildSkillUseResult(opUserSkillPvP, 5, ch.ID, 0, k.SkillID, k.Level, 0, 0, 0), "0x0511 skill out of range")
			return
		}
	}
	sp, mp := s.skillCosts(def, &stats)
	s.startCooldown(k, def)
	s.spendDelta(mp, sp)
	s.persist()
	s.send(buildSkillUsedSpMp(sp, mp), "0x050C skill used sp/mp")
	s.lastCombatAt = time.Now()
	s.log.Printf("Rapture World pvp skill use skill_id=%d level=%d effect=%d detail=%d target_type=%d",
		k.SkillID, k.Level, def.TypeEffect, def.TypeDetail, def.TargetType)
	if def.TargetType == targetTypeEnemiesNearCaster || def.TargetType == targetTypeEnemiesNearTarget {
		x, z, announced := ch.PosX, ch.PosZ, uint32(0)
		if center != nil {
			x, z = center.PositionXZ()
			announced = center.CharacterID
		}
		sendNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius,
			buildSkillUseResult(opUserSkillPvP, resultAnnounce, ch.ID, announced, k.SkillID, k.Level, 0, 0, 0), "0x0511 area skill")
		if def.TypeDetail == detailDrain {
			return
		}
		s.areaUserHits(def, k, x, z)
		for _, id := range mobsWithin(ch.MapID, x, z, float32(def.ApplyRange)) {
			s.skillStrike(id, def, k, false)
		}
		s.afterSkillStrikes(def)
		return
	}
	if def.TypeDetail == detailMulti {
		sendNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius,
			buildSkillUseResult(opUserSkillPvP, resultAnnounce, ch.ID, center.CharacterID, k.SkillID, k.Level, 0, 0, 0), "0x0511 multi-hit skill")
		s.queueSkillHits(center, def, false, def.MultiAttack)
		return
	}
	s.queueSkillHits(center, def, true, 1)
	s.afterSkillStrikes(def)
}

// areaUserHits queues an area skill on the hostile users around (x, z).
func (s *Session) areaUserHits(def *db.SkillDef, k db.CharacterSkill, x, z float32) {
	n := uint8(1)
	if def.TypeDetail == detailMulti {
		n = def.MultiAttack
	}
	for _, p := range s.hostileUsersWithin(x, z, float32(def.ApplyRange)) {
		s.queueSkillHits(p, def, false, n)
	}
}

// afterSkillStrikes is detail 23's sacrifice of the caster after the hits.
func (s *Session) afterSkillStrikes(def *db.SkillDef) {
	if def.TypeDetail == detailSacrifice && s.char.CurrentHP > 0 {
		s.sacrifice()
	}
}

func (s *Session) queueSkillHits(p *ConnectedPlayer, def *db.SkillDef, first bool, n uint8) {
	ch := s.char
	stats := s.stats()
	caster := [3]uint32{stats.CurrentHP, stats.CurrentSP, stats.CurrentMP}
	for i := uint8(0); i < n; i++ {
		p.QueueUserHit(pendingUserHit{AttackerID: ch.ID, Stats: stats, Kind: int(def.Kind), X: ch.PosX, Z: ch.PosZ,
			Skill: def, First: first, Caster: caster})
	}
}

// applyUserSkillHit is the target side of CUser::AttackUser(skill); true
// when this character died.
func (s *Session) applyUserSkillHit(h pendingUserHit, stats CharacterStats) bool {
	ch := s.char
	sk := h.Skill
	attacker := FindPlayerByID(h.AttackerID)
	drain := h.First && sk.TypeDetail == detailDrain
	op := opUserSkillOther
	switch {
	case drain:
		op = opUserSkillDrain
	case h.First:
		op = opUserSkillPvP
	}
	send := func(result uint8, dealt, healed [3]uint16, label string) {
		var frame []byte
		if op == opUserSkillDrain {
			frame = buildDrainPacket(op, result, h.AttackerID, ch.ID, sk.SkillID, sk.SkillLevel, dealt, healed)
		} else {
			frame = buildSkillUseResult(op, result, h.AttackerID, ch.ID, sk.SkillID, sk.SkillLevel, dealt[0], dealt[1], dealt[2])
		}
		sendNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius, frame, label)
	}
	s.cancelMove()
	kind := min(int(sk.Kind), 2)
	if s.blocksAttack(kind) || !userSkillHits(&h.Stats, &stats, sk) {
		send(attackResultMiss, [3]uint16{}, [3]uint16{}, "pvp skill (miss)")
		return false
	}
	duelHitBy(s.player, h.AttackerID) // PvPChkLeaveByAttack
	closeRanged := DistanceXZ(h.X, h.Z, ch.PosX, ch.PosZ) <= closeRangeDistance
	mirrored := s.reflects(kind, nil)
	damage, crit := userSkillDamage(&h.Stats, &stats, sk, h.Caster, closeRanged)
	if crit && attacker != nil {
		attacker.QueueWeaponWear()
	}
	words := func(d [3]int32) [3]uint16 {
		var out [3]uint16
		for i, v := range d {
			out[i] = uint16(min(max(v, 0), 65535))
		}
		return out
	}
	dealt := words(damage)
	hp := uint32(dealt[0])
	if stats.Absorption > 0 && hp > 0 {
		hp = max(satSub(hp, stats.Absorption), 1)
	}
	code := uint8(attackSuccessNormal)
	if crit {
		code = attackResultCritical
	}
	killed := false
	if !mirrored {
		killed = s.takeUserSkillDamage(h.AttackerID, [3]uint32{hp, uint32(dealt[1]), uint32(dealt[2])})
		if drain && attacker != nil {
			attacker.QueueUserHit(pendingUserHit{AttackerID: ch.ID, DrainHeal: true, Heal: [3]uint32{uint32(dealt[0]), uint32(dealt[1]), uint32(dealt[2])}})
		}
	}
	shown := dealt
	if mirrored {
		shown = [3]uint16{}
	}
	healed := [3]uint16{}
	if drain && !mirrored {
		healed = dealt
	}
	send(code, shown, healed, "pvp skill")
	if !drain && !killed && (sk.AddDamageHP != 0 || sk.AddDamageSP != 0 || sk.AddDamageMP != 0) {
		add := [3]uint32{uint32(uint16(sk.AddDamageHP)), uint32(uint16(sk.AddDamageSP)), uint32(uint16(sk.AddDamageMP))}
		killed = s.takeUserSkillDamage(h.AttackerID, add)
		sendNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius,
			buildUserSkillExtra(h.AttackerID, ch.ID, sk.SkillID, sk.SkillLevel, add), "0x0514 pvp skill extra damage")
	}
	if mirrored && attacker != nil {
		attacker.QueueUserHit(pendingUserHit{AttackerID: ch.ID, Reflected: true,
			Damage: [3]uint32{uint32(dealt[0]), uint32(dealt[1]), uint32(dealt[2])}})
	}
	if !killed && sk.TypeEffect == typeEffectDebuff {
		if _, ok := s.addEffect(sk, 0); ok {
			s.buffs[len(s.buffs)-1].CasterUser = h.AttackerID
		}
	}
	s.log.Printf("Rapture World pvp skill hit attacker_id=%d target_id=%d skill_id=%d damage=%v crit=%t mirrored=%t killed=%t",
		h.AttackerID, ch.ID, sk.SkillID, dealt, crit, mirrored, killed)
	s.afterUserDamage(h.AttackerID, killed)
	if !killed {
		s.itemOptionsAfterUserHit(h)
	}
	return killed
}

// takeUserSkillDamage is CUser::Damage with SP and MP damage too.
func (s *Session) takeUserSkillDamage(attackerID uint32, d [3]uint32) bool {
	if d[1] > 0 || d[2] > 0 {
		s.spendDelta(d[2], d[1])
	}
	if d[0] == 0 {
		s.persist()
		return false
	}
	return s.takeUserDamage(attackerID, d[0])
}

// applyDrainHeal is CUser::Heal on the drain's caster.
func (s *Session) applyDrainHeal(h pendingUserHit) {
	if s.char == nil || s.char.CurrentHP == 0 {
		return
	}
	s.healDelta(h.Heal[0], h.Heal[2], h.Heal[1])
	s.persist()
	st := s.stats()
	s.send(buildCurrentHitpoints(&st), "0x0521 current hp after drain")
}

// buildUserSkillExtra (0x0514): caster, target, skill, level, the
// AddDamage HP/SP/MP.
func buildUserSkillExtra(caster, target uint32, skillID uint16, level uint8, add [3]uint32) []byte {
	w := packet.New(opUserSkillExtra)
	w.U32(caster)
	w.U32(target)
	w.U16(skillID)
	w.U8(level)
	for _, v := range add {
		w.U16(uint16(v))
	}
	return w.MustFinish()
}
