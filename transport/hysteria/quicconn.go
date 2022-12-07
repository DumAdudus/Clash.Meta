// Modified from: https://github.com/HyNetwork/hysteria
// License: MIT

package hysteria

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"net"

	"github.com/HyNetwork/hysteria/pkg/utils"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
)

var (
	_ net.Conn       = &quicStream{}
	_ net.PacketConn = &quicPktConn{}
)

type quicStream struct {
	quic.Stream
	PseudoLocalAddr  net.Addr
	PseudoRemoteAddr net.Addr
	Established      bool
}

func (s *quicStream) Read(b []byte) (n int, err error) {
	if !s.Established {
		var sr serverResponse
		err := struc.Unpack(s.Stream, &sr)
		if err != nil {
			_ = s.Close()
			return 0, err
		}
		if !sr.OK {
			_ = s.Close()
			return 0, fmt.Errorf("connection rejected: %s", sr.Message)
		}
		s.Established = true
	}
	return s.Stream.Read(b)
}

func (s *quicStream) LocalAddr() net.Addr {
	return s.PseudoLocalAddr
}

func (s *quicStream) RemoteAddr() net.Addr {
	return s.PseudoRemoteAddr
}

type quicPktConn struct {
	quic.Connection
	quic.Stream
	CloseFunc    func()
	UDPSessionID uint32
	MsgCh        <-chan *udpMessage
}

func (c *quicPktConn) Hold() {
	// Hold the stream until it's closed
	buf := make([]byte, 1024)
	for {
		_, err := c.Stream.Read(buf)
		if err != nil {
			break
		}
	}
	_ = c.Close()
}

func (c *quicPktConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	msg := <-c.MsgCh
	if msg == nil {
		// Closed
		return 0, nil, errors.New("closed")
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

func (c *quicPktConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
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
	var msgBuf bytes.Buffer
	_ = struc.Pack(&msgBuf, &msg)
	err = c.Connection.SendMessage(msgBuf.Bytes())
	if err != nil {
		if errSize, ok := err.(quic.ErrMessageToLarge); ok {
			// need to frag
			msg.MsgID = uint16(rand.Intn(0xFFFF)) + 1 // msgID must be > 0 when fragCount > 1
			fragMsgs := fragUDPMessage(msg, int(errSize))
			for _, fragMsg := range fragMsgs {
				msgBuf.Reset()
				_ = struc.Pack(&msgBuf, &fragMsg)
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

func (c *quicPktConn) Close() error {
	c.CloseFunc()
	return c.Stream.Close()
}
