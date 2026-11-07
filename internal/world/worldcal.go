package world

import (
	"sync"
	"time"

	"github.com/Spelunkern/Rapture/internal/db"
	"github.com/Spelunkern/Rapture/internal/logx"
)

// The world calendar follows ps_dbAgent.exe / ps_game.exe: the base is
// PS_GameData.dbo.WorldInfo.LastWorldTime, packed as (year - 2000) << 26 |
// month << 22 | day << 17 | hour << 12 | minute << 6 | second and created
// from the current local time on the first start (usp_Read_WorldInfo_E,
// usp_Save_WorldInfo_E). The DB agent's SetWorldDay sends the seconds
// elapsed since it (SubTimeSecond, truncated); CWorld::GetWorldDay adds the
// seconds since then, and character entry sends that as 0x0404.

var (
	worldCalMu      sync.Mutex
	worldBase       = time.Date(2020, 1, 1, 12, 30, 0, 0, time.Local)
	worldElapsedAt  time.Time
	worldElapsedSec uint32
)

func packWorldTime(t time.Time) int32 {
	y := max(min(t.Year(), 2063), 2000) - 2000
	return int32(uint32(y)<<26 | uint32(t.Month())<<22 | uint32(t.Day())<<17 |
		uint32(t.Hour())<<12 | uint32(t.Minute())<<6 | uint32(t.Second()))
}

func unpackWorldTime(v int32) time.Time {
	u := uint32(v)
	return time.Date(2000+int(u>>26), time.Month((u>>22)&15), int((u>>17)&31),
		int((u>>12)&31), int((u>>6)&63), int(u&63), 0, time.Local)
}

// InitWorldCalendar loads (or creates) the world base and samples the
// elapsed seconds, as the DB agent does at connection.
func InitWorldCalendar(log *logx.Logger) {
	now := time.Now()
	base := now
	packed, err := db.ReadWorldInfo()
	switch {
	case err != nil:
		log.Printf("Rapture World world calendar load failed: %v", err)
	case packed != 0:
		base = unpackWorldTime(packed)
	default:
		if err := db.SaveWorldInfo(packWorldTime(now)); err != nil {
			log.Printf("Rapture World world calendar save failed: %v", err)
		}
	}
	worldCalMu.Lock()
	worldBase, worldElapsedAt = base, now
	worldElapsedSec = uint32(int64(now.Sub(base) / time.Second))
	worldCalMu.Unlock()
	log.Printf("Rapture World world calendar base=%s elapsed=%ds", base.Format(time.DateTime), worldElapsedSec)
}

// worldDay is CWorld::GetWorldDay.
func worldDay() uint32 {
	worldCalMu.Lock()
	defer worldCalMu.Unlock()
	if worldElapsedAt.IsZero() {
		return 0
	}
	return worldElapsedSec + uint32(time.Since(worldElapsedAt)/time.Second)
}

// worldBasePacked is the stored base, for creating the WorldInfo row.
func worldBasePacked() int32 {
	worldCalMu.Lock()
	defer worldCalMu.Unlock()
	return packWorldTime(worldBase)
}
