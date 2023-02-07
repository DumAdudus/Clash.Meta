// Modified from: https://github.com/HyNetwork/hysteria
// License: MIT

package hysteria

import (
	"math/rand"
	"net"

	"github.com/Dreamacro/clash/log"
	"github.com/apernet/hysteria/core/utils"
	"github.com/lunixbochs/struc"
	"github.com/quic-go/quic-go"
	"github.com/valyala/bytebufferpool"
)

var (
	_       net.Conn       = &hyTCPConn{}
	_       net.PacketConn = &hyUDPConn{}
	holdBuf                = make([]byte, 512)
)

type hyTCPConn struct {
	quic.Stream
	PseudoLocalAddr  net.Addr
	PseudoRemoteAddr net.Addr
	Established      bool
}

func (s *hyTCPConn) Read(b []byte) (n int, err error) {
	if !s.Established {
		var sr serverResponse
		err = HandleServerResp(s.Stream, &sr)
		if err != nil {
			return 0, err
		}
		s.Established = true
	}
	return s.Stream.Read(b)
}

func (s *hyTCPConn) LocalAddr() net.Addr {
	return s.PseudoLocalAddr
}

func (s *hyTCPConn) RemoteAddr() net.Addr {
	return s.PseudoRemoteAddr
}

type hyUDPConn struct {
	quic.Connection
	quic.Stream
	UdpSession   *udpSession
	UDPSessionID uint32
	MsgCh        <-chan *udpMessage
}

func (c *hyUDPConn) Hold() {
	// Hold the stream until it's closed
	for {
		n, err := c.Stream.Read(holdBuf)
		log.Debugln("hysteria: UDP stream hold %v bytes", n)
		if err != nil {
			break
		}
	}
	_ = c.Close()
}

func (c *hyUDPConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	msg := <-c.MsgCh
	if msg == nil {
		// Closed
		return 0, nil, ErrConnClosed
	}
	ip, zone := utils.ParseIPZone(msg.Host)
	addr = &net.UDPAddr{
		IP:   ip,
		Port: int(msg.Port),
		Zone: zone,
	}
	n = copy(p, msg.Data)
	return
}

func (c *hyUDPConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	host, port, err := utils.SplitHostPort(addr.String())
	if err != nil {
		return
	}
	msg := udpMessage{
		SessionID: c.UDPSessionID,
		Host:      host,
		Port:      port,
		FragCount: 1,
		Data:      p,
	}
	// try no frag first
	msgBuf := bytebufferpool.Get()
	defer bytebufferpool.Put(msgBuf)
	_ = struc.Pack(msgBuf, &msg)
	err = c.Connection.SendMessage(msgBuf.Bytes())
	if err != nil {
		if errSize, ok := err.(quic.ErrMessageTooLarge); ok {
			// need to frag
			msg.MsgID = uint16(rand.Intn(0xFFFF)) + 1 // msgID must be > 0 when fragCount > 1
			fragMsgs := fragUDPMessage(msg, int(errSize))
			for _, fragMsg := range fragMsgs {
				msgBuf.Reset()
				_ = struc.Pack(msgBuf, &fragMsg)
				err = c.Connection.SendMessage(msgBuf.Bytes())
				if err != nil {
					return
				}
			}
		} else {
			// some other error
			return
		}
	}

	n = len(p)
	return
}

func (c *hyUDPConn) Close() error {
	c.UdpSession.CloseSession(c.UDPSessionID)
	return c.Stream.Close()
}
