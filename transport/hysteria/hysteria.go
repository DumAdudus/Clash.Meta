package hysteria

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/congestion"
	"github.com/lunixbochs/struc"
	hycongestion "github.com/tobyxdd/hysteria/pkg/congestion"
	"github.com/tobyxdd/hysteria/pkg/conns/udp"
	"github.com/tobyxdd/hysteria/pkg/conns/wechat"
	"github.com/tobyxdd/hysteria/pkg/obfs"
	"github.com/tobyxdd/hysteria/pkg/pmtud_fix"
	"github.com/tobyxdd/hysteria/pkg/utils"
)

type CongestionFactory func(refBPS uint64) congestion.CongestionControl

var (
	ErrClosed = errors.New("closed")

	ccBrutal = func(refBPS uint64) congestion.CongestionControl {
		return hycongestion.NewBrutalSender(congestion.ByteCount(refBPS))
	}

	defaultCongestionControl = ccBrutal
)

type Client struct {
	context           context.Context
	udpconn           *net.UDPConn
	serverAddr        string
	protocol          string
	sendBPS, recvBPS  uint64
	auth              []byte
	congestionFactory CongestionFactory
	obfuscator        obfs.Obfuscator

	tlsConfig  *tls.Config
	quicConfig *quic.Config

	quicSession    quic.Connection
	reconnectMutex sync.Mutex
	closed         bool

	udpSessionMutex sync.RWMutex
	udpSessionMap   map[uint32]chan *udpMessage
	udpDefragger    defragger
}

func NewClient(serverAddr string, protocol string, auth []byte, tlsConfig *tls.Config, quicConfig *quic.Config,
	sendBPS uint64, recvBPS uint64, obfuscator obfs.Obfuscator,
) (*Client, error) {
	quicConfig.DisablePathMTUDiscovery = quicConfig.DisablePathMTUDiscovery || pmtud_fix.DisablePathMTUDiscovery
	c := &Client{
		serverAddr:        serverAddr,
		protocol:          protocol,
		sendBPS:           sendBPS,
		recvBPS:           recvBPS,
		auth:              auth,
		congestionFactory: defaultCongestionControl,
		obfuscator:        obfuscator,
		tlsConfig:         tlsConfig,
		quicConfig:        quicConfig,
	}

	return c, nil
}

func (c *Client) connectToServer() error {
	qs, err := c.QUICDial()
	if err != nil {
		return err
	}
	// Control stream
	stream, err := qs.OpenStreamSync(c.context)
	if err != nil {
		_ = qs.CloseWithError(closeErrorCodeProtocol, "protocol error")
		return err
	}
	ok, msg, err := c.handleControlStream(qs, stream)
	if err != nil {
		_ = qs.CloseWithError(closeErrorCodeProtocol, "protocol error")
		return err
	}
	if !ok {
		_ = qs.CloseWithError(closeErrorCodeAuth, "auth error")
		return fmt.Errorf("auth error: %s", msg)
	}
	// All good
	c.udpSessionMap = make(map[uint32]chan *udpMessage)
	go c.handleMessage(qs)
	c.quicSession = qs
	return nil
}

func (c *Client) handleControlStream(qs quic.Connection, stream quic.Stream) (bool, string, error) {
	// Send protocol version
	_, err := stream.Write([]byte{protocolVersion})
	if err != nil {
		return false, "", err
	}
	// Send client hello
	err = struc.Pack(stream, &clientHello{
		Rate: transmissionRate{
			SendBPS: c.sendBPS,
			RecvBPS: c.recvBPS,
		},
		Auth: c.auth,
	})
	if err != nil {
		return false, "", err
	}
	// Receive server hello
	var sh serverHello
	err = struc.Unpack(stream, &sh)
	if err != nil {
		return false, "", err
	}
	// Set the congestion accordingly
	if sh.OK && c.congestionFactory != nil {
		qs.SetCongestionControl(c.congestionFactory(sh.Rate.RecvBPS))
	}
	return sh.OK, sh.Message, nil
}

func (c *Client) handleMessage(qs quic.Connection) {
	for {
		msg, err := qs.ReceiveMessage()
		if err != nil {
			break
		}
		var udpMsg udpMessage
		err = struc.Unpack(bytes.NewBuffer(msg), &udpMsg)
		if err != nil {
			continue
		}
		dfMsg := c.udpDefragger.Feed(udpMsg)
		if dfMsg == nil {
			continue
		}
		c.udpSessionMutex.RLock()
		ch, ok := c.udpSessionMap[dfMsg.SessionID]
		if ok {
			select {
			case ch <- dfMsg:
				// OK
			default:
				// Silently drop the message when the channel is full
			}
		}
		c.udpSessionMutex.RUnlock()
	}
}

func (c *Client) openStreamWithReconnect() (quic.Connection, quic.Stream, error) {
	c.reconnectMutex.Lock()
	defer c.reconnectMutex.Unlock()
	if c.closed {
		return nil, nil, ErrClosed
	}

	if c.quicSession == nil {
		if err := c.connectToServer(); err != nil {
			return nil, nil, err
		}
	}

	stream, err := c.quicSession.OpenStream()
	if err == nil {
		// All good
		return c.quicSession, &wrappedQUICStream{stream}, nil
	}
	// Something is wrong
	if nErr, ok := err.(net.Error); ok && nErr.Temporary() {
		// Temporary error, just return
		return nil, nil, err
	}
	// Permanent error, need to reconnect
	if err := c.connectToServer(); err != nil {
		// Still error, oops
		return nil, nil, err
	}
	// We are not going to try again even if it still fails the second time
	stream, err = c.quicSession.OpenStream()
	return c.quicSession, &wrappedQUICStream{stream}, err
}

func (c *Client) DialTCP(context context.Context, addr string) (net.Conn, error) {
	host, port, err := utils.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	session, stream, err := c.openStreamWithReconnect()
	if err != nil {
		return nil, err
	}
	// Send request
	err = struc.Pack(stream, &clientRequest{
		UDP:  false,
		Host: host,
		Port: port,
	})
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	// Read response
	var sr serverResponse
	err = struc.Unpack(stream, &sr)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	if !sr.OK {
		_ = stream.Close()
		return nil, fmt.Errorf("connection rejected: %s", sr.Message)
	}
	return &quicConn{
		Orig:             stream,
		PseudoLocalAddr:  session.LocalAddr(),
		PseudoRemoteAddr: session.RemoteAddr(),
	}, nil
}

func (c *Client) DialUDP() (net.PacketConn, error) {
	session, stream, err := c.openStreamWithReconnect()
	if err != nil {
		return nil, err
	}
	// Send request
	err = struc.Pack(stream, &clientRequest{
		UDP: true,
	})
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	// Read response
	var sr serverResponse
	err = struc.Unpack(stream, &sr)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	if !sr.OK {
		_ = stream.Close()
		return nil, fmt.Errorf("connection rejected: %s", sr.Message)
	}

	// Create a session in the map
	c.udpSessionMutex.Lock()
	nCh := make(chan *udpMessage, 1024)
	// Store the current session map for CloseFunc below
	// to ensures that we are adding and removing sessions on the same map,
	// as reconnecting will reassign the map
	sessionMap := c.udpSessionMap
	sessionMap[sr.UDPSessionID] = nCh
	c.udpSessionMutex.Unlock()

	pktConn := &quicPktConn{
		Session: session,
		Stream:  stream,
		CloseFunc: func() {
			c.udpSessionMutex.Lock()
			if ch, ok := sessionMap[sr.UDPSessionID]; ok {
				close(ch)
				delete(sessionMap, sr.UDPSessionID)
			}
			c.udpSessionMutex.Unlock()
		},
		UDPSessionID: sr.UDPSessionID,
		MsgCh:        nCh,
	}
	go pktConn.Hold()
	return pktConn, nil
}

func (c *Client) Close() error {
	c.reconnectMutex.Lock()
	defer c.reconnectMutex.Unlock()
	err := c.quicSession.CloseWithError(closeErrorCodeGeneric, "")
	c.closed = true
	return err
}

func (c *Client) UDPSetup(ctx context.Context, conn *net.UDPConn) error {
	c.context = ctx
	c.udpconn = conn
	return nil
}

func (c *Client) quicPacketConn() (net.PacketConn, error) {
	var pktConn net.PacketConn = c.udpconn

	if len(c.protocol) == 0 || c.protocol == "udp" {
		if c.obfuscator != nil {
			pktConn = udp.NewObfsUDPConn(c.udpconn, c.obfuscator)
		}
	} else if c.protocol == "wechat-video" {
		if c.obfuscator != nil {
			pktConn = wechat.NewObfsWeChatUDPConn(c.udpconn, c.obfuscator)
		}
	} else {
		return nil, fmt.Errorf("unsupported protocol: %s", c.protocol)
	}

	return pktConn, nil
}

func (c *Client) QUICDial() (quic.Connection, error) {
	serverUDPAddr, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return nil, err
	}
	pktConn, err := c.quicPacketConn()
	if err != nil {
		return nil, err
	}
	qs, err := quic.Dial(pktConn, serverUDPAddr, c.serverAddr, c.tlsConfig, c.quicConfig)
	if err != nil {
		_ = pktConn.Close()
		return nil, err
	}
	return qs, nil
}
