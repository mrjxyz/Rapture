package world

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Spelunkern/Rapture/internal/logx"
)

type nopLog struct{}

func (nopLog) Printf(string, ...any) {}

// withTiers installs a two-tier table per country for a test.
func withTiers(t *testing.T) {
	kcMu.Lock()
	oldTiers, oldWorld := kcTiers, kcWorld
	var a, b kcTier
	a.Kill, b.Kill = 2, 4
	a.Status[1], a.Status[4] = 10, 5                   // +10% HP, +5 STR
	b.Status[1], b.Status[4], b.Status[10] = 10, 5, 20 // tiers keep the earlier values
	kcTiers = [2][]kcTier{{a, b}, {a, b}}
	kcMu.Unlock()
	now := time.Now()
	kcMu.Lock()
	setKillCountLocked(0, 0, now)
	setKillCountLocked(1, 0, now)
	kcMu.Unlock()
	t.Cleanup(func() {
		kcMu.Lock()
		kcTiers, kcWorld = oldTiers, oldWorld
		kcMu.Unlock()
	})
}

// CWorld::AddKillCount: tiers by count, the top tier holds and resets the
// other country; UpdateKillCount resets it after 10 minutes.
func TestKillCountTiers(t *testing.T) {
	withTiers(t)
	AddKillCount(0, 1, nopLog{})
	if st := kcStatus(0); st[1] != 0 {
		t.Fatal("a tier applies below the first count")
	}
	AddKillCount(1, 3, nopLog{})
	AddKillCount(0, 1, nopLog{})
	if st := kcStatus(0); st[1] != 10 || st[4] != 5 || st[10] != 0 {
		t.Fatalf("tier 0 status %v", st[:11])
	}
	AddKillCount(0, 2, nopLog{}) // 4: top tier
	kcMu.Lock()
	light, dark, hold := kcWorld[0].count, kcWorld[1].count, kcWorld[0].resetAt
	kcMu.Unlock()
	if light != 4 || dark != 0 || hold.IsZero() {
		t.Fatalf("after the top tier light=%d dark=%d hold=%v", light, dark, hold)
	}
	AddKillCount(1, 5, nopLog{})
	if kcWorld[1].count != 0 {
		t.Fatal("kills counted during the hold")
	}
	TickKillCount(hold.Add(time.Second), nopLog{})
	if kcWorld[0].count != 0 || !kcWorld[0].resetAt.IsZero() {
		t.Fatalf("hold did not reset: %d", kcWorld[0].count)
	}
}

// KillStatus.SData: 30 tiers per country, each keeping the earlier
// tiers' values.
func TestLoadKillStatus(t *testing.T) {
	dir := filepath.Join("..", "..", "data", "game")
	if _, err := os.Stat(filepath.Join(dir, "KillStatus.SData")); err != nil {
		t.Skip("no game data")
	}
	t.Chdir(filepath.Join("..", ".."))
	log, err := logx.New("rapture_world_test")
	if err != nil {
		t.Fatal(err)
	}
	kcMu.Lock()
	old := kcTiers
	kcMu.Unlock()
	defer func() { kcMu.Lock(); kcTiers = old; kcMu.Unlock() }()
	if err := loadKillStatus(log); err != nil {
		t.Fatal(err)
	}
	if len(kcTiers[0]) != 30 || len(kcTiers[1]) != 30 || kcTiers[0][0].Kill != 150 {
		t.Fatalf("tiers %d/%d first %d", len(kcTiers[0]), len(kcTiers[1]), kcTiers[0][0].Kill)
	}
	// Tier 0 has 11/12 = 5; tier 10 (1650) raises them to 10 and tier 2
	// adds 10 = 5, kept by every later tier.
	last := kcTiers[0][29].Status
	if last[11] != 10 || last[10] != 10 || last[25] != 100 || last[24] != 5 {
		t.Fatalf("top tier status %v", last)
	}
}

// The tier's percents land on the pools and the recovery.
func TestKillCountStats(t *testing.T) {
	withTiers(t)
	AddKillCount(0, 4, nopLog{})
	base := StatsFor(nil, EquipmentStats{})
	var e EquipmentStats
	applyKCStatus(&e, 0)
	st := StatsFor(nil, e)
	if st.MaxHP != base.MaxHP+kcPercent(base.MaxHP, 10) || st.Str != base.Str+5 {
		t.Fatalf("pools/stats %d->%d str %d->%d", base.MaxHP, st.MaxHP, base.Str, st.Str)
	}
	if want := recoverBaseStanding + kcPercent(st.MaxHP, 20); st.Recover[1][0] != want {
		t.Fatalf("standing HP recovery %d, want %d", st.Recover[1][0], want)
	}
}

// CUser::UpdateRecover / InitEquipment: 28 flat sitting and 4 standing per
// pool, nothing in combat, the sitting amount truncated.
func TestRecoveryBase(t *testing.T) {
	st := StatsFor(nil, EquipmentStats{GemRecover: [3]uint32{3, 2, 1}})
	if st.Recover[0] != [3]uint32{31, 30, 29} || st.Recover[1] != [3]uint32{7, 6, 5} || st.Recover[2] != [3]uint32{} {
		t.Fatalf("recover %v", st.Recover)
	}
	// 5% of 1010 = 50.5 + 28 -> 78 (chopped, not rounded).
	if got := sittingRecovery(1010, 0, 28); got != 78 {
		t.Fatalf("sitting recovery %d, want 78", got)
	}
}
