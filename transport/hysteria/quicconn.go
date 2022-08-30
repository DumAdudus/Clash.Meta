package hysteria

import (
	"bytes"
	"math/rand"
	"net"
	"time"

	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
	"github.com/tobyxdd/hysteria/pkg/utils"
)

var (
	_ net.Conn       = &quicConn{}
	_ net.PacketConn = &quicPktConn{}
)

type quicConn struct {
	Orig             quic.Stream
	PseudoLocalAddr  net.Addr
	PseudoRemoteAddr net.Addr
}

func (w *quicConn) Read(b []byte) (n int, err error) {
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

func (c *quicPktConn) SetDeadline(time.Time) error {
	return nil
}

func (c *quicPktConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *quicPktConn) SetWriteDeadline(time.Time) error {
	return nil
}
