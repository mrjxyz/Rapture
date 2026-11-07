package world

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/Spelunkern/Rapture/internal/db"
	"github.com/Spelunkern/Rapture/internal/packet"
)

// Normal attacks between users follow ps_game.exe:
//
//   - PacketPC 0x0502 and CUser::UpdateAttack 0x0048ca50 pick the target:
//     a user on the zone, alive, not the attacker, and hostile
//     (CUser::IsTeam 0x0044bdf0 false). EnableAttackRange 0x0044bf90 is the
//     move allowance + the attacker's range + 2.3 (no target size).
//   - CUser::AttackUser 0x00456870: the category is ranged for weapon
//     types 11, 13 and 14 and does nothing while disabled; a miss sends
//     0x0502 result 3; a hit runs ChkMirror on the target, AttackDamage,
//     wears the weapon only on a critical (QualityByAttack is behind the
//     crit flag), then CUser::Damage on the target, or, when mirrored,
//     0x0502 with no damage followed by Damage on the attacker and 0x0515.
//   - CUser::AttackSuccess 0x00456b10 and AttackDamage 0x00457030 (user
//     target) differ from the mob versions: the hit chance clamps to 1..99,
//     and damage has no level factor (attack - guard + variance, element,
//     LUC/10, critical 1..99 from CriticalSuccess 0x00456fd0).
//   - Death(target, 1, attacker) announces 0x0504 type 1.
//
// The death's rewards are in pvpdeath.go. Not covered yet: duels and
// guild duels (IsTeam's PvP states), AttackItemOption /
// BeAttackedItemOption, and hostile skills (the skill CUser::AttackUser
// 0x0044f4c0).

const (
	deathTypeUser    = 0x01
	userAttackRange  = 2.3
	pvpCritReduction = 0.034
)

// pendingUserHit is a user's normal attack (or a mirrored one coming back)
// to resolve in the target's session.
type pendingUserHit struct {
	AttackerID uint32
	Stats      CharacterStats // the attacker's, at the swing
	Kind       int
	X, Z       float32 // the attacker's position
	Reflected  bool    // a mirrored hit: Damage is final
	Damage     [3]uint32
	Skill      *db.SkillDef // a hostile skill (pvpskill.go), nil for a normal attack
	First      bool         // the skill's selected target (0x0511/0x0512), else 0x0513
	Caster     [3]uint32    // the caster's HP/SP/MP (DamageType 3)
	DrainHeal  bool         // a drain's heal coming back to its caster
	Heal       [3]uint32
	ItemCast   bool // cast by a gem (m_bItemAttack): casts no gems itself
	GemAttack  bool // run AttackItemOption on AttackerID (the target hit)
}

func (p *ConnectedPlayer) QueueUserHit(h pendingUserHit) {
	p.mailMu.Lock()
	p.userHits = append(p.userHits, h)
	p.mailMu.Unlock()
}

func (p *ConnectedPlayer) TakeUserHits() []pendingUserHit {
	p.mailMu.Lock()
	defer p.mailMu.Unlock()
	h := p.userHits
	p.userHits = nil
	return h
}

// isTeam is CUser::IsTeam without duels (none exist yet): the user
// itself, anyone on a WarType P map, or the same country.
// isTeam is CUser::IsTeam: the user itself, else not the duel opponent,
// then a WarType P map or the same country.
func (s *Session) isTeam(p *ConnectedPlayer) bool {
	if p == s.player {
		return true
	}
	if s.player != nil && (duelFoes(s.player, p) || guildDuelFoes(s.player, p)) {
		return false
	}
	if z, ok := mapConfig.Zones[realMap(s.char.MapID)]; ok && z.WarType == "P" {
		return true
	}
	return p.Faction() == s.faction
}

// hostileUser finds an attackable user for a normal attack.
func (s *Session) hostileUser(id uint32) *ConnectedPlayer {
	if s.char == nil || id == s.char.ID {
		return nil
	}
	p := FindPlayerByID(id)
	if p == nil || p.MapID() != s.char.MapID || !p.IsAlive() || s.isTeam(p) {
		return nil
	}
	return p
}

// userHits is CUser::AttackSuccess against a user for a normal attack.
func userHits(att, def *CharacterStats, kind int) bool {
	if kind == attackMagic {
		return !magicResisted(int32(att.Level), int32(def.Level), def.Evasion[2], int32(att.Wis), int32(def.Wis), att.Accuracy[2])
	}
	level := float64(float32(att.Level))
	levels := float64(float32(int32(att.Level) + int32(def.Level)))
	levelPart := level / levels * 160
	defense := f32(def.Accuracy[kind]) / f32(att.Evasion[kind]+def.Accuracy[kind]) * 100
	accuracy := f32(att.Accuracy[kind]) / f32(def.Evasion[kind]+att.Accuracy[kind]) * 100
	chance := clampInt(truncInt(levelPart-(defense-accuracy)), 1, 99)
	return rand.Int32N(100) < chance
}

// userDamage is CUser::AttackDamage against a user for a normal attack.
func userDamage(att, def *CharacterStats, kind int, closeRanged bool) (int32, bool) {
	var damage [3]int32
	crit := userWeaponDamage(att, def, kind, &damage, att.AttackAttr, def.DefenseAttr, closeRanged)
	return damageLimits(damage[0]), crit
}

// userWeaponDamage is the weapon part of CUser::AttackDamage 0x00457030
// against a user; damage[0] seeds it (skill DamageType 1). Melee and
// ranged: attack - guard + variance, the close-range bow cut, element,
// LUC/10, critical. Magic: the magic attack reduced by a level term
// (1..50) plus the target's magic guard over the attacker's weapon attack
// + INT (1..70) percent, element, LUC/10, critical. Every melee/ranged
// step stores an integer; the magic steps keep floats.
func userWeaponDamage(att, def *CharacterStats, kind int, damage *[3]int32, element, defElement uint8, closeRanged bool) bool {
	luckRoll := func() int32 {
		if att.Luc == 0 {
			return 0
		}
		return int32(rand.Uint32N(att.Luc)) + 1
	}
	factor := float32(1)
	if element < 9 && defElement < 9 {
		factor = attackElement[element][defElement]
	}
	critChance := func() bool {
		c := clampInt(truncInt(f32(att.Critical)/100*100-f32(int32(def.Luc))*float64(float32(pvpCritReduction))), 1, 99)
		return rand.Int32N(100) < c
	}
	if kind == attackMagic {
		attack := store(f32(att.Attack[attackMagic] + damage[0]))
		levelRaw := (int32(def.Level)-int32(att.Level)-1)*100 + int32(def.Level)
		level := store(f32(levelRaw) / f32(int32(att.Level)+int32(def.Level)) * 2.4)
		level = min(max(level, 1), 50)
		resist := store(f32(def.Guard[attackMagic]) / f32(int32(att.ItemAttack)+int32(att.Int)) * 100)
		if !(resist >= 1) { // NaN or below 1
			resist = 1
		}
		resist = min(resist, 70)
		sum := store(float64(resist) + float64(level))
		attack = store(float64(factor) * float64(attack))
		attack = store(float64(attack) - float64(attack)/100*float64(sum))
		if attack < 0 {
			attack = 0
		}
		attack = store(f32(luckRoll())*float64(float32(0.1)) + float64(attack))
		crit := critChance()
		if crit {
			attack = store(f32(luckRoll())*float64(float32(1.4)) + float64(attack)*0.5 + float64(attack))
		}
		damage[0] = truncInt(float64(attack))
		return crit
	}
	value := damage[0] + att.Attack[kind] - def.Guard[kind]
	if att.Variance > 0 && int32(att.Variance) > 0 {
		value += int32(rand.Uint32N(att.Variance))
	}
	value = max(value, 0)
	if kind == attackRanged && att.CrossRoad == 0 && closeRanged {
		value = truncInt(f32(value) * float64(float32(0.7)))
	}
	value = truncInt(f32(value) * float64(factor))
	value = truncInt(f32(luckRoll())*float64(float32(0.1)) + f32(value))
	crit := critChance()
	if crit {
		value = truncInt(f32(luckRoll())*float64(float32(1.4)) + f32(value)*0.5 + f32(value))
		// The crit branch repeats the close-range cut without the
		// cross-road check.
		if kind == attackRanged && closeRanged {
			value = truncInt(f32(value) * float64(float32(0.7)))
		}
	}
	damage[0] = value
	return crit
}

// userSkillDamage is CUser::AttackDamage against a user with a skill, the
// same DamageType modes as on a mob (0 raw, 1 seeds the weapon damage, 2
// multiplies it, 3 the caster's own pools, 4/5 a percentage of the
// target's maximum/current pools) on the user-target weapon formula.
func userSkillDamage(att, def *CharacterStats, sk *db.SkillDef, caster [3]uint32, closeRanged bool) ([3]int32, bool) {
	values := [3]int32{int32(uint16(sk.DamageHP)), int32(uint16(sk.DamageSP)), int32(uint16(sk.DamageMP))}
	var damage [3]int32
	capAll := func() {
		for i := range damage {
			damage[i] = min(damage[i], maxDamage)
		}
	}
	switch sk.DamageType {
	case 0:
		return values, false
	case 3:
		for i, v := range values {
			if v != 0 {
				damage[i] = max(int32(caster[i]*uint32(v)), 1)
			}
		}
		capAll()
		return damage, false
	case 4, 5:
		source := [3]uint32{def.MaxHP, def.MaxSP, def.MaxMP}
		if sk.DamageType == 5 {
			source = [3]uint32{def.CurrentHP, def.CurrentSP, def.CurrentMP}
		}
		for i, v := range values {
			if v != 0 {
				product := math.Trunc(f32(v) / 100 * float64(float32(source[i])))
				damage[i] = max(int32(uint32(int64(product))), 1)
			}
		}
		capAll()
		return damage, false
	case 1:
		damage = values
	}
	element := att.AttackAttr
	if sk.AttrType != 0 {
		element = sk.AttrType
	}
	defElement := def.DefenseAttr
	if sk.TypeDetail == detailNoElement {
		defElement = 0
	}
	crit := userWeaponDamage(att, def, int(sk.Kind), &damage, element, defElement, closeRanged)
	if sk.DamageType == 2 {
		for i, v := range values {
			if v != 0 {
				damage[i] = int32(uint32(damage[i]) * uint32(v))
			}
		}
	}
	capAll()
	if damage[0] < 11 {
		damage[0] = rand.Int32N(10) + 1
	}
	return damage, crit
}

// userSkillHits is CUser::AttackSuccess 0x00456b10 with a skill:
// SuccessType 1 rolls SuccessValue, 2 and 3 add LUC or WIS as an integer
// ratio, otherwise the category's normal chance (a category above magic
// always hits). The target's HP-ability shields (7/8) and state
// resistances are not modeled.
func userSkillHits(att, def *CharacterStats, sk *db.SkillDef) bool {
	if sk.SuccessType >= 1 && sk.SuccessType <= 3 {
		chance := int32(sk.SuccessValue)
		if sk.SuccessType != 1 {
			mine, theirs := int32(att.Luc), int32(def.Luc)
			if sk.SuccessType == 3 {
				mine, theirs = int32(att.Wis), int32(def.Wis)
			}
			if mine+theirs != 0 {
				chance += mine / (mine + theirs) * 100
			}
		}
		return rand.Int32N(100) < chance
	}
	if sk.Kind > attackMagic {
		return true
	}
	return userHits(att, def, int(sk.Kind))
}

// userAttackSwing is one UpdateAttack step on a user target; false ends
// the auto-attack.
func (s *Session) userAttackSwing(target uint32) bool {
	ch := s.char
	if ch == nil || s.mounted || s.actionBlocked() {
		return false
	}
	wc, ok := equippedWeaponCombat(s.items)
	if !ok {
		return false
	}
	stats := StatsFor(ch, s.equipment())
	if idx := min(max(int32(wc.AttackTime)-1+stats.AttackSpeed, 0), 9); idx != int32(wc.AttackTime)-1 {
		wc.Delay, _ = weaponAttackDelay(uint8(idx + 1))
	}
	if !s.lastAutoAttackAt.IsZero() && time.Since(s.lastAutoAttackAt) < wc.Delay {
		return true
	}
	p := s.hostileUser(target)
	if p == nil {
		return false
	}
	x, z := p.PositionXZ()
	distance := DistanceXZ(ch.PosX, ch.PosZ, x, z)
	if distance > wc.Range+float32(stats.Range)+userAttackRange+autoAttackRangeGrace {
		if distance > autoAttackCancelRange {
			return false
		}
		return true
	}
	kind := attackMelee
	if stats.RangedWeapon {
		kind = attackRanged
	}
	if stats.Disabled[kind] > 0 {
		return true
	}
	s.lastAutoAttackAt, s.lastCombatAt = time.Now(), time.Now()
	p.QueueUserHit(pendingUserHit{AttackerID: ch.ID, Stats: stats, Kind: kind, X: ch.PosX, Z: ch.PosZ})
	return true
}

// tickUserHits resolves the user attacks queued on this session.
func (s *Session) tickUserHits() {
	if s.player == nil {
		return
	}
	for _, h := range s.player.TakeUserHits() {
		if s.dead {
			return
		}
		if s.applyUserHit(h) {
			s.dead, s.autoAttack, s.cast = true, false, nil
			s.lastAutoAttackAt = time.Time{}
			s.clearBuffsOnDeath()
		}
	}
}

// applyUserHit is the target side of CUser::AttackUser; true when this
// character died.
func (s *Session) applyUserHit(h pendingUserHit) bool {
	ch := s.char
	if ch == nil {
		return false
	}
	stats := s.stats()
	if stats.CurrentHP == 0 {
		return false
	}
	s.lastCombatAt = time.Now()
	if h.DrainHeal {
		s.applyDrainHeal(h)
		return false
	}
	if h.GemAttack {
		s.applyGemAttack(h.AttackerID)
		return false
	}
	if h.Skill != nil {
		return s.applyUserSkillHit(h, stats)
	}
	attacker := FindPlayerByID(h.AttackerID)
	if h.Reflected {
		killed := s.takeUserSkillDamage(h.AttackerID, h.Damage)
		w := packet.New(0x0515)
		w.U32(h.AttackerID) // the mirror's owner
		w.U32(ch.ID)
		for _, v := range h.Damage {
			w.U16(uint16(min(v, 65535)))
		}
		sendNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius, w.MustFinish(), "0x0515 mirrored hit")
		s.afterUserDamage(h.AttackerID, killed)
		return killed
	}
	s.cancelMove() // StatusCastingStop on the target
	if s.blocksAttack(h.Kind) || !userHits(&h.Stats, &stats, h.Kind) {
		sendNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius,
			buildCharacterUserAttack(h.AttackerID, ch.ID, attackResultMiss, 0), "0x0502 user attack (miss)")
		return false
	}
	duelHitBy(s.player, h.AttackerID) // PvPChkLeaveByAttack
	closeRanged := DistanceXZ(h.X, h.Z, ch.PosX, ch.PosZ) <= closeRangeDistance
	mirrored := s.reflects(h.Kind, nil)
	value, crit := userDamage(&h.Stats, &stats, h.Kind, closeRanged)
	if crit && attacker != nil {
		attacker.QueueWeaponWear()
	}
	damage := uint32(max(value, 0))
	if stats.Absorption > 0 {
		damage = max(satSub(damage, stats.Absorption), 1)
	}
	code := uint8(attackSuccessNormal)
	if crit {
		code = attackResultCritical
	}
	killed, shown := false, damage
	if mirrored {
		shown = 0
	} else {
		killed = s.takeUserDamage(h.AttackerID, damage)
	}
	sendNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius,
		buildCharacterUserAttack(h.AttackerID, ch.ID, code, uint16(min(shown, 65535))), "0x0502 user attack")
	if mirrored && attacker != nil {
		attacker.QueueUserHit(pendingUserHit{AttackerID: ch.ID, Reflected: true, Damage: [3]uint32{damage, 0, 0}})
	}
	s.log.Printf("Rapture World user attack attacker_id=%d target_id=%d kind=%d damage=%d crit=%t mirrored=%t killed=%t",
		h.AttackerID, ch.ID, h.Kind, damage, crit, mirrored, killed)
	s.afterUserDamage(h.AttackerID, killed)
	if !killed {
		s.itemOptionsAfterUserHit(h)
	}
	return killed
}

// takeUserDamage is CUser::Damage's HP part; true when it killed.
func (s *Session) takeUserDamage(attackerID, damage uint32) bool {
	ch := s.char
	cur := s.stats()
	if cur.CurrentHP == 0 || damage == 0 {
		return false
	}
	s.wearArmour()
	s.addDamage(damageByUser, attackerID, damage) // CDamage::Add in CUser::Damage
	after := satSub(cur.CurrentHP, damage)
	ch.CurrentHP = satSub(ch.CurrentHP, cur.CurrentHP-after)
	killed := after == 0
	s.persist()
	s.dismount()
	if !killed {
		s.wake()
	}
	return killed
}

func (s *Session) afterUserDamage(killerID uint32, killed bool) {
	ch := s.char
	refreshed := s.stats()
	s.send(buildCurrentHitpoints(&refreshed), "0x0521 current hp after user attack")
	if !killed {
		return
	}
	s.deathByUser(killerID)
	defer duelLeave(s.player, 3, true)     // PvPChkLeaveByDeath
	defer guildDuelLeave(s.player, nil, 3) // its guild part
	state, death := buildCharacterDeathState(ch.ID), buildCharacterDeath(ch.ID, deathTypeUser, killerID)
	for _, p := range PlayersNear(ch.MapID, ch.PosX, ch.PosZ, CombatRadius) {
		p.Send(state, "0x051C character death state")
		p.Send(death, "0x0504 character death (by user)")
	}
	if s.player != nil {
		s.player.SetAlive(false)
	}
	s.log.Printf("Rapture World character died character_id=%d killer_character_id=%d", ch.ID, killerID)
}

func buildCharacterUserAttack(attackerID, targetID uint32, result uint8, damage uint16) []byte {
	w := packet.New(packet.CharacterUserAutoAttack)
	w.U8(result)
	w.U32(attackerID)
	w.U32(targetID)
	w.U16(damage)
	w.U16(0)
	w.U16(0)
	return w.MustFinish()
}

// QueueWeaponWear asks the owner's session for one QualityByAttack.
func (p *ConnectedPlayer) QueueWeaponWear() {
	p.mailMu.Lock()
	p.weaponWear++
	p.mailMu.Unlock()
}

func (p *ConnectedPlayer) TakeWeaponWear() int {
	p.mailMu.Lock()
	defer p.mailMu.Unlock()
	n := p.weaponWear
	p.weaponWear = 0
	return n
}
