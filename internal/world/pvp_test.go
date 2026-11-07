package world

import "testing"

// CUser::AttackDamage 0x00457030 against a user has no level factor:
// attack - guard (+ variance), element, LUC/10, critical x1.5 (+LUC*1.4).
func TestUserDamageFormula(t *testing.T) {
	att := CharacterStats{Level: 10, Critical: 0}
	att.Attack[attackMelee] = 500
	def := CharacterStats{Level: 60}
	def.Guard[attackMelee] = 100
	for i := 0; i < 500; i++ {
		v, crit := userDamage(&att, &def, attackMelee, false)
		want := int32(400)
		if crit {
			want = 600
		}
		if v != want {
			t.Fatalf("damage %d crit=%v, want %d", v, crit, want)
		}
	}
	// A bow within 3 units loses 30% without the cross-road passive.
	att.Attack[attackRanged] = 500
	def.Guard[attackRanged] = 100
	if v, crit := userDamage(&att, &def, attackRanged, true); !crit && v != 279 {
		t.Fatalf("close ranged damage %d, want 279 (float32 0.7)", v)
	}
	att.CrossRoad = 1
	if v, crit := userDamage(&att, &def, attackRanged, true); !crit && v != 400 {
		t.Fatalf("cross-road close ranged damage %d, want 400", v)
	}
	// Guard above attack floors at the 1..10 random minimum.
	def.Guard[attackMelee] = 900
	if v, _ := userDamage(&att, &def, attackMelee, false); v < 1 || v > 10 {
		t.Fatalf("floored damage %d", v)
	}
}

// CUser::AttackSuccess 0x00456b10 clamps the user hit chance to 1..99
// (the mob version floors at 20).
func TestUserHitChanceClamp(t *testing.T) {
	weak := CharacterStats{Level: 1}
	weak.Accuracy[0], weak.Evasion[0] = 1, 1
	strong := CharacterStats{Level: 80}
	strong.Accuracy[0], strong.Evasion[0] = 900, 900
	hits := 0
	for i := 0; i < 20000; i++ {
		if userHits(&weak, &strong, attackMelee) {
			hits++
		}
	}
	if hits == 0 || hits > 600 {
		t.Fatalf("weak attacker hit %d/20000, want about 1%%", hits)
	}
	hits = 0
	for i := 0; i < 20000; i++ {
		if userHits(&strong, &weak, attackMelee) {
			hits++
		}
	}
	if hits < 19400 || hits == 20000 {
		t.Fatalf("strong attacker hit %d/20000, want about 99%%", hits)
	}
}
