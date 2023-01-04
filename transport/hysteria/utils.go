package hysteria

import (
	"context"
	"fmt"
	"net"

	"github.com/Dreamacro/clash/component/dialer"
	"github.com/apernet/hysteria/core/pktconns/obfs"
	"github.com/apernet/hysteria/core/pktconns/udp"
	"github.com/apernet/hysteria/core/pktconns/wechat"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
)

const sysConnBuffer = 1024 * 1024 * 4 // 4MB buffer

func SendServerReq(s quic.Stream, r *clientRequest) error {
	err := struc.Pack(s, r)
	if err != nil {
		_ = s.Close()
	}
	return err
}

func HandleServerResp(s quic.Stream, sr *serverResponse) error {
	err := struc.Unpack(s, sr)
	if err != nil {
		_ = s.Close()
		return err
	}
	if !sr.OK {
		_ = s.Close()
		return fmt.Errorf("connection rejected: %s", sr.Message)
	}
	return nil
}

func GetPacketConn(ctx context.Context, protocol string, obfs obfs.Obfuscator) (net.PacketConn, error) {
	pktConn, _ := dialer.ListenPacket(ctx, "udp", "", []dialer.Option{}...)
	udpConn, _ := pktConn.(*net.UDPConn)
	udpConn.SetReadBuffer(sysConnBuffer)
	udpConn.SetWriteBuffer(sysConnBuffer)

	if len(protocol) == 0 || protocol == udp.Protocol {
		if obfs != nil {
			pktConn = udp.NewObfsUDPConn(udpConn, obfs)
		}
	} else if protocol == wechat.Protocol {
		pktConn = wechat.NewObfsWeChatUDPConn(udpConn, obfs)
	} else {
		return nil, fmt.Errorf("unsupported protocol: %s", protocol)
	}

	return pktConn, nil
}
