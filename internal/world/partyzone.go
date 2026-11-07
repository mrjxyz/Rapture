package world

import (
	"sync"
	"time"

	"github.com/Spelunkern/Rapture/internal/packet"
)

// Party dungeons (Map.ini CreateType P) and instance expiry follow
// ps_game.exe:
//
//   - CUser::PacketMaininterface 0x020A (portal) to a P map: closed outside
//     its CreateTime slots (CMap::IsPartyInsZoneOpenTime, all 7 entries;
//     none means always open) -> 0x020A (portal, 9); without a party -> 4;
//     the party's instance full (CWorld::FindPartyInsZone: users + waiting
//     >= EnterMaxUser) -> 11; fewer members than CreateMinUser -> 10.
//   - CWorld::GetZoneByMapID: one instance per party (owner = party id),
//     created on first entry.
//   - CWorldThread::Update: an instance with nobody in it gets an expiry of
//     ExpireTime minutes + 30 s the first time it is empty (not reset when
//     users come back) and is destroyed once empty past it.

const (
	portalClosed      = 9
	portalNoParty     = 4
	portalPartySmall  = 10
	portalPartyFull   = 11
	partyCreateType   = "P"
	houseCreateType   = "H"
	instanceSweepTick = time.Second
)

// partyOpen is IsPartyInsZoneOpenTime.
func partyOpen(z ZoneConfig, now time.Time) bool {
	return len(z.Schedule) == 0 || grbOpen(z.Schedule, now)
}

// findInstance is the zone of mapID owned by owner.
func findInstance(mapID uint16, owner uint32) (uint16, bool) {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	for zone, m := range instanceMaps {
		if m == mapID && instanceOwner[zone] == owner {
			return zone, true
		}
	}
	return 0, false
}

// partyID returns the user's party id and member count (0 without one).
func partyOfPlayer(p *ConnectedPlayer) (uint32, int) {
	partyMu.Lock()
	defer partyMu.Unlock()
	if pt := partyOf(p); pt != nil {
		return pt.id, len(pt.members)
	}
	return 0, 0
}

// portalGate is the 0x020A checks for an instanced target map; 0 lets the
// user through.
func (s *Session) portalGate(target uint16) uint8 {
	z, ok := mapConfig.Zones[target]
	if !ok || z.CreateType != partyCreateType {
		return 0
	}
	if !partyOpen(z, grbNow()) {
		return portalClosed
	}
	id, members := partyOfPlayer(s.player)
	if id == 0 {
		return portalNoParty
	}
	if zone, ok := findInstance(target, id); ok && zone != s.char.MapID && len(PlayersOnMap(zone)) >= z.EnterMaxUser {
		return portalPartyFull
	}
	if members < z.CreateMinUser {
		return portalPartySmall
	}
	return 0
}

func buildPortalResult(index, result uint8) []byte {
	w := packet.New(packet.CharacterEnteredPortal)
	w.U8(index)
	w.U8(result)
	return w.MustFinish()
}

// enterPartyInstance is GetZoneByMapID's party case.
func (s *Session) enterPartyInstance(mapID uint16) (uint16, bool) {
	id, _ := partyOfPlayer(s.player)
	if id == 0 {
		return 0, false
	}
	if zone, ok := findInstance(mapID, id); ok {
		return zone, true
	}
	zone, err := createInstance(mapID, id)
	if err != nil {
		s.log.Printf("Rapture World party instance failed: %v", err)
		return 0, false
	}
	s.log.Printf("Rapture World party instance zone=%d map=%d party=%d", zone, mapID, id)
	return zone, true
}

var (
	instanceExpiryMu sync.Mutex
	instanceExpiry   = map[uint16]time.Time{}
)

// TickInstances is CWorldThread::Update's instance expiry.
func TickInstances(now time.Time, log logPrinter) {
	instanceMu.Lock()
	zones := make(map[uint16]uint16, len(instanceMaps))
	for z, m := range instanceMaps {
		zones[z] = m
	}
	instanceMu.Unlock()
	instanceExpiryMu.Lock()
	defer instanceExpiryMu.Unlock()
	for z := range instanceExpiry {
		if _, ok := zones[z]; !ok {
			delete(instanceExpiry, z)
		}
	}
	for zone, m := range zones {
		if len(PlayersOnMap(zone)) > 0 {
			continue
		}
		at, set := instanceExpiry[zone]
		if !set {
			instanceExpiry[zone] = now.Add(mapConfig.Zones[m].Expire)
			continue
		}
		if !now.Before(at) {
			delete(instanceExpiry, zone)
			destroyInstance(zone)
			log.Printf("Rapture World instance zone=%d map=%d expired", zone, m)
		}
	}
}
