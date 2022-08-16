// Modified from: https://github.com/HyNetwork/hysteria
// License: MIT

package hysteria

import (
	"bytes"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/HyNetwork/hysteria/pkg/utils"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
)

var (
	_ net.Conn       = &quicConn{}
	_ net.PacketConn = &quicPktConn{}
)

type quicConn struct {
	Orig             quic.Stream
	PseudoLocalAddr  net.Addr
	PseudoRemoteAddr net.Addr
	Established      bool
}

func (w *quicConn) Read(b []byte) (n int, err error) {
	if !w.Established {
		var sr serverResponse
		err := struc.Unpack(w.Orig, &sr)
		if err != nil {
			_ = w.Close()
			return 0, err
		}
		if !sr.OK {
			_ = w.Close()
			return 0, fmt.Errorf("connection rejected: %s", sr.Message)
		}
		w.Established = true
	}
	return w.Orig.Read(b)
}

func (w *quicConn) Write(b []byte) (n int, err error) {
	return w.Orig.Write(b)
}

func (w *quicConn) Close() error {
	return w.Orig.Close()
}

func (w *quicConn) LocalAddr() net.Addr {
	return w.PseudoLocalAddr
}

func (w *quicConn) RemoteAddr() net.Addr {
	return w.PseudoRemoteAddr
}

func (w *quicConn) SetDeadline(t time.Time) error {
	return w.Orig.SetDeadline(t)
}

func (w *quicConn) SetReadDeadline(t time.Time) error {
	return w.Orig.SetReadDeadline(t)
}

func (w *quicConn) SetWriteDeadline(t time.Time) error {
	return w.Orig.SetWriteDeadline(t)
}

type quicPktConn struct {
	Session      quic.Connection
	Stream       quic.Stream
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
		return 0, nil, ErrClosed
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
	err = c.Session.SendMessage(msgBuf.Bytes())
	if err != nil {
		if errSize, ok := err.(quic.ErrMessageToLarge); ok {
			// need to frag
			msg.MsgID = uint16(rand.Intn(0xFFFF)) + 1 // msgID must be > 0 when fragCount > 1
			fragMsgs := fragUDPMessage(msg, int(errSize))
			for _, fragMsg := range fragMsgs {
				msgBuf.Reset()
				_ = struc.Pack(&msgBuf, &fragMsg)
				err = c.Session.SendMessage(msgBuf.Bytes())
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

func (c *quicPktConn) LocalAddr() net.Addr {
	return c.Session.LocalAddr()
}

func (c *quicPktConn) SetDeadline(t time.Time) error {
	return c.Stream.SetDeadline(t)
}

func (c *quicPktConn) SetReadDeadline(t time.Time) error {
	return c.Stream.SetReadDeadline(t)
}

func (c *quicPktConn) SetWriteDeadline(t time.Time) error {
	return c.Stream.SetWriteDeadline(t)
}