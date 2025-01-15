package messagecheck

import "github.com/TerraDharitri/drt-go-chain-core/core"

type p2pSigner interface {
	Verify(payload []byte, pid core.PeerID, signature []byte) error
}
