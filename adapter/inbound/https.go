package inbound

import (
	"net"
	"net/http"

	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/context"
)

// NewHTTPS receive CONNECT request and return ConnContext
func NewHTTPS(request *http.Request, conn net.Conn) *context.ConnContext {
	metadata := parseHTTPAddr(request)
	metadata.Type = C.HTTPS
	if ip, port, err := parseAddr(conn.RemoteAddr().String()); err == nil {
		metadata.SrcIP = ip
		metadata.SrcPort = port
	}

	metadata.RawSrcAddr = conn.RemoteAddr()
	metadata.RawDstAddr = conn.LocalAddr()

	return context.NewConnContext(conn, metadata)
}
