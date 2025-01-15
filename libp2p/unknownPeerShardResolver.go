package libp2p

import (
	"github.com/TerraDharitri/drt-go-chain-core/core"
	p2p "github.com/TerraDharitri/drt-go-chain-p2p"
)

var _ p2p.PeerShardResolver = (*unknownPeerShardResolver)(nil)

type unknownPeerShardResolver struct {
}

// GetPeerInfo returns a P2PPeerInfo value holding an unknown peer value
func (upsr *unknownPeerShardResolver) GetPeerInfo(_ core.PeerID) core.P2PPeerInfo {
	return core.P2PPeerInfo{
		PeerType:    core.UnknownPeer,
		PeerSubType: core.RegularPeer,
		ShardID:     0,
	}
}

// IsInterfaceNil returns true if there is no value under the interface
func (upsr *unknownPeerShardResolver) IsInterfaceNil() bool {
	return upsr == nil
}
