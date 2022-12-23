package hysteria

import "errors"

var (
	ErrMaxSend    = errors.New("max send reached")
	ErrConnClosed = errors.New("QUIC conn closed")
	ErrNoConn     = errors.New("no conn")
)
