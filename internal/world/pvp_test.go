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

// CUser::GetExp 0x004581b0: dead level x 50 x ((dead - killer + 1) x 0.2
// + 1), at least 1, 0 when the dead user is 10+ levels above.
func TestPvPExp(t *testing.T) {
	cases := []struct {
		dead, killer uint16
		want         uint32
	}{
		{40, 40, 2400}, // 2000 x 1.2
		{49, 40, 7350}, // diff 9: 2450 x 3
		{50, 40, 0},    // diff 10
		{20, 40, 1},    // a negative factor floors at 1
		{30, 35, 600},  // 1500 x 0.2
	}
	for _, c := range cases {
		got := pvpExp(c.dead, c.killer)
		diff := float64(int32(c.dead) - int32(c.killer))
		want := uint32(max(int64(float64(c.dead)*50*((diff+1)*0.2+1)), 1))
		if diff > 9 {
			want = 0
		}
		if got != want {
			t.Fatalf("pvpExp(%d, %d) = %d, want %d", c.dead, c.killer, got, want)
		}
	}
}

func TestPvPDropChance(t *testing.T) {
	for vg, want := range map[int16]int{0: 100, 10: 100, 11: 95, -51: 90, 200: 65, -191: 65, 171: 70, 141: 80, 101: 85} {
		if got := pvpDropChance(vg); got != want {
			t.Fatalf("Vg %d: %d%%, want %d%%", vg, got, want)
		}
	}
}

// CDamage::GetMax keeps the first entry on a tie.
func TestDamageLedgerTop(t *testing.T) {
	s := &Session{}
	s.addDamage(damageByMob, 7, 50)
	s.addDamage(damageByUser, 1, 30)
	s.addDamage(damageByUser, 1, 20)
	s.addDamage(damageByUser, 2, 10)
	top, ok := s.topDamage()
	if !ok || top.Type != damageByMob || top.ID != 7 {
		t.Fatalf("top %+v, want the mob (first of the 50s)", top)
	}
	s.addDamage(damageByUser, 2, 41)
	if top, _ = s.topDamage(); top.Type != damageByUser || top.ID != 2 || top.Amount != 51 {
		t.Fatalf("top %+v, want user 2 with 51", top)
	}
}

// The magic branch of AttackDamage 0x00457030 against a user: equal
// levels give the level term's floor 1; guard 100 over weapon attack 0 +
// INT 200 resists 50%: 1000 x (1 - 51/100) = 490 (735 on a critical).
func TestUserMagicDamage(t *testing.T) {
	att := CharacterStats{Level: 60, Int: 200}
	att.Attack[attackMagic] = 1000
	def := CharacterStats{Level: 60}
	def.Guard[attackMagic] = 100
	for i := 0; i < 300; i++ {
		var d [3]int32
		crit := userWeaponDamage(&att, &def, attackMagic, &d, 0, 0, false)
		want := int32(490)
		if crit {
			want = 735
		}
		if d[0] != want {
			t.Fatalf("magic damage %d crit=%v, want %d", d[0], crit, want)
		}
	}
}
