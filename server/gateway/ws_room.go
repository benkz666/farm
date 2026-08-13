package gateway

import (
	"context"
	"time"
)

// Room state is transport state only. Farm decides whether access is allowed
// and returns subscribe/unsubscribe directives in its typed response.
func (connection *wsConnection) currentRoom() uint64 {
	if connection == nil {
		return 0
	}
	connection.roomMu.Lock()
	defer connection.roomMu.Unlock()
	return connection.roomUID
}

func (connection *wsConnection) setRoomWatermark(ownerUID, farmSeq uint64) {
	if connection == nil || ownerUID == 0 {
		return
	}
	connection.roomMu.Lock()
	if connection.roomUID == ownerUID {
		connection.roomSeq = farmSeq
		connection.roomSeqKnown = true
		connection.roomSeqObservedAt = time.Now().UnixNano()
	}
	connection.roomMu.Unlock()
}

func (connection *wsConnection) observeRoomDeltaLocked(farmSeq uint64) {
	if farmSeq == 0 || !connection.roomSeqKnown || farmSeq != connection.roomSeq+1 {
		connection.roomSeqKnown = false
		return
	}
	connection.roomSeq = farmSeq
	connection.roomSeqObservedAt = time.Now().UnixNano()
}

func (connection *wsConnection) advanceRoomWatermark(ownerUID, farmSeq uint64) {
	if connection == nil || ownerUID == 0 || farmSeq == 0 {
		return
	}
	connection.roomMu.Lock()
	defer connection.roomMu.Unlock()
	if connection.roomUID != ownerUID || !connection.roomSeqKnown {
		return
	}
	if farmSeq != connection.roomSeq && farmSeq != connection.roomSeq+1 {
		connection.roomSeqKnown = false
		return
	}
	connection.roomSeq = farmSeq
	connection.roomSeqObservedAt = time.Now().UnixNano()
}

// enterRoom installs the ordering barrier before EnterFarm reaches Farm. The
// barrier prevents a concurrent FarmDelta from overtaking the snapshot.
func (g *Gateway) enterRoom(connection *wsConnection, ownerUID uint64) error {
	if connection == nil || ownerUID == 0 || connection.id == 0 {
		return nil
	}
	connection.roomMu.Lock()
	alreadySubscribed := connection.roomUID == ownerUID
	if alreadySubscribed {
		connection.holdFarmDeltas = true
		connection.heldFarmDeltas = nil
		connection.roomSeqKnown = false
	}
	connection.roomMu.Unlock()
	if alreadySubscribed {
		return nil
	}

	if g.connRegistry != nil {
		if err := g.connRegistry.Subscribe(context.Background(), ownerUID, connection.id, g.gatewayID); err != nil {
			return err
		}
	}
	connection.roomMu.Lock()
	previousOwnerUID := connection.roomUID
	connection.roomUID = ownerUID
	connection.roomSeq = 0
	connection.roomSeqKnown = false
	connection.roomSeqObservedAt = 0
	connection.holdFarmDeltas = true
	connection.heldFarmDeltas = nil
	connection.roomMu.Unlock()

	if previousOwnerUID != 0 {
		if g.connRegistry != nil {
			_ = g.connRegistry.Unsubscribe(context.Background(), previousOwnerUID, connection.id, g.gatewayID)
		}
	}
	return nil
}

func (g *Gateway) leaveFarm(connection *wsConnection) {
	if connection == nil {
		return
	}
	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomUID = 0
	connection.roomSeq = 0
	connection.roomSeqKnown = false
	connection.roomSeqObservedAt = 0
	connection.holdFarmDeltas = false
	connection.heldFarmDeltas = nil
	connection.roomMu.Unlock()
	if connection.id == 0 || ownerUID == 0 {
		return
	}
	if g.connRegistry != nil {
		_ = g.connRegistry.Unsubscribe(context.Background(), ownerUID, connection.id, g.gatewayID)
	}
}
