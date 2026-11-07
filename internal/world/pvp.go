package world

import (
	"math/rand/v2"
	"time"

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
func (s *Session) isTeam(p *ConnectedPlayer) bool { return s.teamUsable(p) }

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

// userDamage is CUser::AttackDamage against a user for a normal attack
// (melee or ranged). Each step stores to an integer like the original.
func userDamage(att, def *CharacterStats, kind int, closeRanged bool) (int32, bool) {
	luckRoll := func() int32 {
		if att.Luc == 0 {
			return 0
		}
		return int32(rand.Uint32N(att.Luc)) + 1
	}
	damage := att.Attack[kind] - def.Guard[kind]
	if att.Variance > 0 && int32(att.Variance) > 0 {
		damage += int32(rand.Uint32N(att.Variance))
	}
	damage = max(damage, 0)
	if kind == attackRanged && att.CrossRoad == 0 && closeRanged {
		damage = truncInt(f32(damage) * float64(float32(0.7)))
	}
	factor := float32(1)
	if att.AttackAttr < 9 && def.DefenseAttr < 9 {
		factor = attackElement[att.AttackAttr][def.DefenseAttr]
	}
	damage = truncInt(f32(damage) * float64(factor))
	damage = truncInt(f32(luckRoll())*float64(float32(0.1)) + f32(damage))
	critChance := clampInt(truncInt(f32(att.Critical)/100*100-f32(int32(def.Luc))*float64(float32(pvpCritReduction))), 1, 99)
	crit := rand.Int32N(100) < critChance
	if crit {
		bonus := f32(luckRoll()) * float64(float32(1.4))
		damage = truncInt(bonus + f32(damage)*0.5 + f32(damage))
		// The crit branch repeats the close-range cut without the
		// cross-road check.
		if kind == attackRanged && closeRanged {
			damage = truncInt(f32(damage) * float64(float32(0.7)))
		}
	}
	return damageLimits(damage), crit
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
	attacker := FindPlayerByID(h.AttackerID)
	if h.Reflected {
		killed := s.takeUserDamage(h.AttackerID, h.Damage[0])
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
