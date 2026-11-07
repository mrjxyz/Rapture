package world

import (
	"github.com/Spelunkern/Rapture/internal/db"
)

// Gem skills follow CUser::AttackItemOption 0x0044c630 / 0x0044c720 and
// BeAttackedItemOption 0x0044c970 / 0x0044cae0: a gem whose Special is
// 1-15 (16 is the recovery gem) names a mob skill (the Skills row with
// that id at level 100). After a landed hit:
//
//   - user on user (normal or skill): the attacker's weapon gems cast on
//     the target and the target's armour gems on the attacker;
//   - user on mob: the attacker's weapon gems cast on the mob;
//   - mob on user: the user's armour gems cast on the mob.
//
// Armour is read in the order shield, helmet, upper, gloves, lower, shoes
// and stops at the first empty slot. TypeEffect 1 casts as a selected
// attack (SkillAttackSelect), 3 as a debuff; each skill rolls its own
// success. m_bItemAttack keeps a gem cast from casting gems again, and
// both sides must be alive.

const (
	gemSkillLevel  = 100
	maxGemSkillSpc = 15
)

var gemArmourOrder = [6]uint8{6, 0, 1, 3, 2, 4}

func gemSkillsOf(it *db.Item) []*db.SkillDef {
	defs := db.MustDefs()
	var out []*db.SkillDef
	for _, g := range it.Gems {
		if g == 0 {
			continue
		}
		d := defs.Item(gemItemType, g)
		if d == nil || d.Special == 0 || d.Special > maxGemSkillSpc {
			continue
		}
		if sk := defs.Skill(uint16(d.Special), gemSkillLevel); sk != nil &&
			(sk.TypeEffect == typeEffectAttack || sk.TypeEffect == typeEffectDebuff) {
			out = append(out, sk)
		}
	}
	return out
}

// weaponGemSkills is AttackItemOption's weapon scan.
func weaponGemSkills(items []db.Item) []*db.SkillDef {
	if w := equippedItem(items, weaponSlot); w != nil {
		return gemSkillsOf(w)
	}
	return nil
}

// armourGemSkills is BeAttackedItemOption's armour scan.
func armourGemSkills(items []db.Item) []*db.SkillDef {
	var out []*db.SkillDef
	for _, slot := range gemArmourOrder {
		it := equippedItem(items, slot)
		if it == nil {
			break
		}
		out = append(out, gemSkillsOf(it)...)
	}
	return out
}

func gemCharSkill(sk *db.SkillDef) db.CharacterSkill {
	return db.CharacterSkill{SkillID: sk.SkillID, Level: sk.SkillLevel}
}

// gemCastsOnUser queues this user's gem skills on p.
func (s *Session) gemCastsOnUser(p *ConnectedPlayer, skills []*db.SkillDef) {
	if p == nil || !p.IsAlive() || s.char == nil || s.char.CurrentHP == 0 {
		return
	}
	stats := s.stats()
	caster := [3]uint32{stats.CurrentHP, stats.CurrentSP, stats.CurrentMP}
	for _, sk := range skills {
		p.QueueUserHit(pendingUserHit{AttackerID: s.char.ID, Stats: stats, Kind: int(sk.Kind), X: s.char.PosX, Z: s.char.PosZ,
			Skill: sk, First: true, Caster: caster, ItemCast: true})
	}
}

// gemCastsOnMob casts this user's gem skills on a mob.
func (s *Session) gemCastsOnMob(mobID uint32, skills []*db.SkillDef) {
	if s.itemAttacking || len(skills) == 0 || s.char == nil || s.char.CurrentHP == 0 {
		return
	}
	s.itemAttacking = true
	defer func() { s.itemAttacking = false }()
	for _, sk := range skills {
		if m, ok := MobByID(s.char.MapID, mobID); !ok || m.CurrentHP == 0 {
			return
		}
		s.skillHit(mobID, sk, gemCharSkill(sk), true)
	}
}

// itemOptionsAfterUserHit runs both sides for a landed user-on-user hit,
// in the target's (this) session.
func (s *Session) itemOptionsAfterUserHit(h pendingUserHit) {
	if h.ItemCast || s.char == nil || s.char.CurrentHP == 0 {
		return
	}
	attacker := FindPlayerByID(h.AttackerID)
	if attacker == nil || !attacker.IsAlive() {
		return
	}
	attacker.QueueUserHit(pendingUserHit{AttackerID: s.char.ID, GemAttack: true}) // AttackItemOption in its session
	s.gemCastsOnUser(attacker, armourGemSkills(s.items))                          // BeAttackedItemOption
}

// applyGemAttack is AttackItemOption for this (attacking) user on target.
func (s *Session) applyGemAttack(target uint32) {
	s.gemCastsOnUser(FindPlayerByID(target), weaponGemSkills(s.items))
}
