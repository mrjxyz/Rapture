package world

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Spelunkern/Rapture/internal/db"
	"github.com/Spelunkern/Rapture/internal/logx"
	"github.com/Spelunkern/Rapture/internal/packet"
)

// The country kill count ("God's blessing") follows ps_game.exe:
//
//   - CGameData::LoadKCStatus 0x004bf4a0 reads data/KillStatus.SData:
//     per record a country, a kill count and six (type, value) pairs over
//     types 1-27; each tier then keeps at least every earlier tier's value
//     per type.
//   - CWorld::AddKillCount 0x00413600: each user kill (KillPCTo) adds 1 to
//     the killer's country, unless either country waits at the top tier.
//     Crossing a tier's count moves to the highest tier reached; the top
//     tier holds 10 minutes and resets the other country to 0 at once
//     (ResetKillCount); CWorld::UpdateKillCount then resets this one too.
//     Counts are announced to the country with 0x020F (country, count up
//     to the top tier's) on each change and every 10 minutes, saved with
//     usp_Save_GodBless_E, loaded from usp_Read_GodBless_R, and sent at
//     world entry with 0x0211 (country, count, ms until the reset).
//   - CUser::UpdateKCStatus 0x0048b780 / InitKCStatus: the tier's types on
//     every user of the country. 1-3 add that percent of the maximum
//     HP/MP/SP; 4-9 STR, REC, INT, WIS, DEX, LUC (REC/WIS/DEX with their
//     5 pool points); 10-12 add that percent of the maximum HP/SP/MP to the
//     sitting and standing recovery, 13-15 to the in-combat recovery; 16
//     critical (ability 18); 17 melee accuracy (20); 18/19 that percent of
//     the melee/ranged defense; 20 and 23 magic evasion (31); 21/22 melee
//     and ranged evasion (29/30). Country-wide, 24 adds to the gem chance
//     (ItemGemAdd / ItemGemRemoveAll) and 25 cuts the death exp loss by
//     that percent (ExpDrop). Types 26 and 27 have no reader in EP4.5.
//
// Below the first tier no type applies (the original reads the slot
// before the table there). Obelisk kills (CMap::ObeliskDestroy) are not
// modeled: Rapture has no obelisks.

const (
	kcTypes          = 28
	kcHoldTime       = 10 * time.Minute
	kcBroadcastEvery = 10 * time.Minute
)

type kcTier struct {
	Kill   uint32
	Status [kcTypes]int32
}

type kcCountry struct {
	count       uint32
	index       int // -1 below the first tier
	next        uint32
	resetAt     time.Time // the top tier's hold
	broadcastAt time.Time
}

var (
	kcMu    sync.Mutex
	kcTiers [2][]kcTier
	kcWorld = [2]kcCountry{{index: -1}, {index: -1}}
)

// loadKillStatus is CGameData::LoadKCStatus.
func loadKillStatus(log *logx.Logger) error {
	b, err := os.ReadFile(filepath.Join(gameDataDir(), "KillStatus.SData"))
	if err != nil {
		return err
	}
	if len(b) < 4 {
		return fmt.Errorf("short file")
	}
	n := int(binary.LittleEndian.Uint32(b))
	var tiers [2][]kcTier
	o := 4
	for i := 0; i < n; i++ {
		if o+7+18 > len(b) {
			return fmt.Errorf("record %d truncated", i)
		}
		country := b[o]
		t := kcTier{Kill: binary.LittleEndian.Uint32(b[o+1:])}
		o += 7
		for j := 0; j < 6; j++ {
			typ, v := b[o], binary.LittleEndian.Uint16(b[o+1:])
			o += 3
			if typ != 0 && typ < kcTypes {
				t.Status[typ] = int32(v)
			}
		}
		if country < 2 {
			tiers[country] = append(tiers[country], t)
		}
	}
	for c := range tiers {
		for i := 1; i < len(tiers[c]); i++ {
			for k := range tiers[c][i].Status {
				tiers[c][i].Status[k] = max(tiers[c][i].Status[k], tiers[c][i-1].Status[k])
			}
		}
	}
	kcMu.Lock()
	kcTiers = tiers
	kcMu.Unlock()
	log.Printf("Rapture World loaded KillStatus.SData tiers light=%d dark=%d", len(tiers[0]), len(tiers[1]))
	return nil
}

// kcTierFor is CGameData::GetKCStatusByCount (-1 below the first tier).
func kcTierFor(c int, count uint32) int {
	t := kcTiers[c]
	for i := len(t) - 1; i >= 0; i-- {
		if t[i].Kill <= count {
			return i
		}
	}
	return -1
}

func kcMaxKill(c int) uint32 {
	if t := kcTiers[c]; len(t) > 0 {
		return t[len(t)-1].Kill
	}
	return 0
}

// setKillCountLocked is CWorld::SetKillCount / InitKillCount for c.
func setKillCountLocked(c int, count uint32, now time.Time) {
	w := &kcWorld[c]
	w.count = count
	w.index = kcTierFor(c, count)
	w.next, w.resetAt = 0, time.Time{}
	switch {
	case w.index+1 < len(kcTiers[c]):
		w.next = kcTiers[c][w.index+1].Kill
	case len(kcTiers[c]) > 0:
		w.resetAt = now.Add(kcHoldTime)
	}
	w.broadcastAt = now.Add(kcBroadcastEvery)
}

// InitKillCount loads the saved counts at world start.
func InitKillCount(log *logx.Logger) {
	light, dark, err := db.ReadGodBless()
	if err != nil {
		log.Printf("Rapture World kill count load failed: %v", err)
	}
	kcMu.Lock()
	now := time.Now()
	setKillCountLocked(0, light, now)
	setKillCountLocked(1, dark, now)
	kcMu.Unlock()
	log.Printf("Rapture World country kill counts light=%d dark=%d", light, dark)
}

// AddKillCount is CWorld::AddKillCount for country c.
func AddKillCount(c uint8, n uint32, log logPrinter) {
	if c > 1 {
		return
	}
	kcMu.Lock()
	if !kcWorld[0].resetAt.IsZero() || !kcWorld[1].resetAt.IsZero() {
		kcMu.Unlock()
		return
	}
	now := time.Now()
	w := &kcWorld[c]
	w.count += n
	if w.next == 0 || w.count < w.next {
		kcMu.Unlock()
		return
	}
	w.index = kcTierFor(int(c), w.count)
	maxed := w.index >= len(kcTiers[c])-1
	if maxed {
		w.resetAt = now.Add(kcHoldTime)
	} else {
		w.next = kcTiers[c][w.index+1].Kill
	}
	w.broadcastAt = now.Add(kcBroadcastEvery)
	other := 1 - int(c)
	if maxed {
		setKillCountLocked(other, 0, now)
		kcWorld[other].resetAt = time.Time{}
	}
	light, dark := kcWorld[0].count, kcWorld[1].count
	kcMu.Unlock()
	kcApplyCountry(int(c))
	kcAnnounce(int(c))
	if maxed {
		kcApplyCountry(other)
		kcAnnounce(other)
		kcSave(light, dark, log)
	}
	log.Printf("Rapture World country %d kill count %d reached tier %d (maxed=%t)", c, w.count, w.index, maxed)
}

// TickKillCount is CWorld::UpdateKillCount.
func TickKillCount(now time.Time, log logPrinter) {
	kcMu.Lock()
	var announce, apply []int
	dirty := false
	for c := range kcWorld {
		w := &kcWorld[c]
		switch {
		case w.resetAt.IsZero():
			if !now.Before(w.broadcastAt) {
				announce = append(announce, c)
				w.broadcastAt = now.Add(kcBroadcastEvery)
				dirty = true
			}
		case !now.Before(w.resetAt):
			setKillCountLocked(c, 0, now)
			w.resetAt = time.Time{}
			announce, apply = append(announce, c), append(apply, c)
			dirty = true
		}
	}
	light, dark := kcWorld[0].count, kcWorld[1].count
	kcMu.Unlock()
	for _, c := range apply {
		kcApplyCountry(c)
	}
	for _, c := range announce {
		kcAnnounce(c)
	}
	if dirty {
		kcSave(light, dark, log)
	}
}

// kcSave stores both counts; without a WorldInfo row (the world calendar,
// not ported, creates it in the original) it creates the row first.
func kcSave(light, dark uint32, log logPrinter) {
	err := db.SaveGodBless(light, dark)
	if errors.Is(err, db.ErrNoWorldInfo) {
		if err = db.SaveWorldInfo(shaiyaTime()); err == nil {
			err = db.SaveGodBless(light, dark)
		}
	}
	if err != nil {
		log.Printf("Rapture World kill count save failed: %v", err)
	}
}

// kcAnnounce is CWorldThread::SendAllKillCount: 0x020F to the country.
func kcAnnounce(c int) {
	kcMu.Lock()
	count := min(kcWorld[c].count, kcMaxKill(c))
	kcMu.Unlock()
	w := packet.New(0x020F)
	w.U8(uint8(c))
	w.U32(count)
	frame := w.MustFinish()
	for _, p := range AllPlayers() {
		if int(p.Faction()) == c {
			p.Send(frame, "0x020F country kill count")
		}
	}
}

// kcApplyCountry is ApplyKillCount: every user of c refreshes its stats.
func kcApplyCountry(c int) {
	for _, p := range AllPlayers() {
		if int(p.Faction()) == c {
			p.kcRefresh.Store(true)
		}
	}
}

// buildKCStatus is CUser::SendKCStatus (0x0211).
func buildKCStatus(c uint8) []byte {
	kcMu.Lock()
	var count uint32
	var left int64
	if c < 2 {
		count = kcWorld[c].count
		if r := kcWorld[c].resetAt; !r.IsZero() {
			left = max(time.Until(r).Milliseconds(), 0)
		}
	}
	kcMu.Unlock()
	w := packet.New(0x0211)
	w.U8(c)
	w.U32(count)
	w.U32(uint32(left))
	return w.MustFinish()
}

// kcStatus is the country's current tier types (zero below the first).
func kcStatus(c uint8) [kcTypes]int32 {
	kcMu.Lock()
	defer kcMu.Unlock()
	if c > 1 || kcWorld[c].index < 0 || kcWorld[c].index >= len(kcTiers[c]) {
		return [kcTypes]int32{}
	}
	return kcTiers[c][kcWorld[c].index].Status
}

// kcAbility maps the flat KC types onto SetSkillAbility kinds (same field).
var kcAbility = map[int]uint8{4: 4, 5: 5, 6: 6, 7: 7, 8: 8, 9: 9, 16: 18, 17: 20, 20: 31, 21: 29, 22: 30, 23: 31}

// applyKCStatus adds the country tier's user types to the stats.
func applyKCStatus(e *EquipmentStats, c uint8) {
	st := kcStatus(c)
	for typ, kind := range kcAbility {
		if v := st[typ]; v != 0 {
			applyAbility(e, typeEffectBuff, kind, v)
		}
	}
	e.KCPoolPct = [3]int32{st[1], st[2], st[3]}       // HP, MP, SP
	e.KCRecoverPct = [3]int32{st[10], st[11], st[12]} // HP, SP, MP (sit and stand)
	e.KCCombatPct = [3]int32{st[13], st[14], st[15]}  // HP, SP, MP (in combat)
	e.KCGuardPct = [2]int32{st[18], st[19]}
}

// kcPercent is UpdateKCStatus' (int)((float)(base * value) * 0.01).
func kcPercent(base uint32, v int32) uint32 {
	if v == 0 {
		return 0
	}
	return uint32(max(math.Trunc(float64(float32(int32(base)*v))*float64(float32(0.01))), 0))
}

// kcWorldType is a country-wide type (24 gem chance, 25 exp loss cut).
func kcWorldType(c uint8, typ int) int32 { return kcStatus(c)[typ] }
