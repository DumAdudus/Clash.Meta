// Picked from: https://github.com/HyNetwork/hysteria
// License: MIT

package hysteria

import (
	"github.com/lucas-clemente/quic-go"
)

var _ quic.Stream = &qStream{}

// Handle stream close properly
// Ref: https://github.com/libp2p/go-libp2p-quic-transport/blob/master/stream.go
type qStream struct {
	quic.Stream
}

func (s *qStream) Close() error {
	s.Stream.CancelRead(0)
	return s.Stream.Close()
}
