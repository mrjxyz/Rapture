package world

import (
	"testing"
	"time"
)

// The WorldInfo base format: (year-2000)<<26 | month<<22 | day<<17 |
// hour<<12 | minute<<6 | second.
func TestWorldTimePacking(t *testing.T) {
	at := time.Date(2026, 11, 7, 21, 34, 56, 0, time.Local)
	p := packWorldTime(at)
	if uint32(p) != 26<<26|11<<22|7<<17|21<<12|34<<6|56 || !unpackWorldTime(p).Equal(at) {
		t.Fatalf("packed %#x -> %v", uint32(p), unpackWorldTime(p))
	}
	worldCalMu.Lock()
	worldElapsedAt, worldElapsedSec = time.Now().Add(-3*time.Second), 100
	worldCalMu.Unlock()
	if d := worldDay(); d < 103 || d > 104 {
		t.Fatalf("world day %d, want 103", d)
	}
}
