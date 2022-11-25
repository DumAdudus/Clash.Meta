// Modified from: https://github.com/HyNetwork/hysteria
// License: MIT

package hysteria

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Dreamacro/clash/component/dialer"
	"github.com/Dreamacro/clash/log"
	"github.com/HyNetwork/hysteria/pkg/congestion"
	"github.com/HyNetwork/hysteria/pkg/pmtud"
	"github.com/HyNetwork/hysteria/pkg/transport/pktconns/obfs"
	"github.com/HyNetwork/hysteria/pkg/transport/pktconns/udp"
	"github.com/HyNetwork/hysteria/pkg/transport/pktconns/wechat"
	"github.com/HyNetwork/hysteria/pkg/utils"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
)

var ErrClosed = errors.New("closed")

type Client struct {
	context          context.Context
	serverAddr       string
	protocol         string
	sendBPS, recvBPS uint64
	auth             []byte
	obfuscator       obfs.Obfuscator

	tlsConfig  *tls.Config
	quicConfig *quic.Config

	quicConn       quic.Connection
	reconnectMutex sync.Mutex
	closed         bool
	fastOpen       bool

	udpSessionMutex sync.RWMutex
	udpSessionMap   map[uint32]chan *udpMessage
	udpDefragger    defragger
}

func NewClient(serverAddr string, protocol string, auth []byte, tlsConfig *tls.Config, quicConfig *quic.Config,
	sendBPS uint64, recvBPS uint64, obfuscator obfs.Obfuscator, fastOpen bool,
) (*Client, error) {
	quicConfig.DisablePathMTUDiscovery = quicConfig.DisablePathMTUDiscovery || pmtud.DisablePathMTUDiscovery
	c := &Client{
		serverAddr: serverAddr,
		protocol:   protocol,
		sendBPS:    sendBPS,
		recvBPS:    recvBPS,
		auth:       auth,
		obfuscator: obfuscator,
		tlsConfig:  tlsConfig,
		quicConfig: quicConfig,
		fastOpen:   fastOpen,
	}

	return c, nil
}

func (c *Client) connectToServer() error {
	// Clear previous connection
	if c.quicConn != nil {
		_ = c.quicConn.CloseWithError(0, "")
	}

	log.Infoln("hysteria: client %p connect to server: %s", c, c.serverAddr)

	quicConn, err := c.QUICDial()
	if err != nil {
		return err
	}

	// Control stream
	ctx, ctxCancel := context.WithTimeout(context.Background(), protocolTimeout)
	stream, err := quicConn.OpenStreamSync(ctx)
	ctxCancel()
	if err != nil {
		_ = quicConn.CloseWithError(closeErrorCodeProtocol, "protocol error")
		return err
	}
	ok, msg, err := c.handleControlStream(quicConn, stream)
	if err != nil {
		_ = quicConn.CloseWithError(closeErrorCodeProtocol, "protocol error")
		return err
	}
	if !ok {
		_ = quicConn.CloseWithError(closeErrorCodeAuth, "auth error")
		return fmt.Errorf("auth error: %s", msg)
	}
	// All good
	c.udpSessionMap = make(map[uint32]chan *udpMessage)
	go c.handleMessage(quicConn)
	c.quicConn = quicConn
	return nil
}

func (c *Client) handleControlStream(conn quic.Connection, stream quic.Stream) (bool, string, error) {
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
	if sh.OK {
		conn.SetCongestionControl(congestion.NewBrutalSender(sh.Rate.RecvBPS))
	}
	return sh.OK, sh.Message, nil
}

func (c *Client) handleMessage(conn quic.Connection) {
	for {
		msg, err := conn.ReceiveMessage()
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

	log.Infoln("hysteria: openStream")

	if c.quicConn == nil {
		if err := c.connectToServer(); err != nil {
			return nil, nil, err
		}
	}

	stream, err := c.quicConn.OpenStream()
	if err == nil {
		// All good
		return c.quicConn, &wrappedQUICStream{stream}, nil
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
	stream, err = c.quicConn.OpenStream()
	return c.quicConn, &wrappedQUICStream{stream}, err
}

func (c *Client) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	log.Infoln("hysteria: dialtcp")
	c.context = ctx
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
	// If fast open is enabled, we return the stream immediately
	// and defer the response handling to the first Read() call
	if !c.fastOpen {
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
	}

	return &quicConn{
		Orig:             stream,
		PseudoLocalAddr:  session.LocalAddr(),
		PseudoRemoteAddr: session.RemoteAddr(),
		Established:      !c.fastOpen,
	}, nil
}

func (c *Client) DialUDP(ctx context.Context) (net.PacketConn, error) {
	log.Infoln("hysteria: dialudp")
	c.context = ctx
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
	log.Infoln("hysteria: close")
	c.reconnectMutex.Lock()
	defer c.reconnectMutex.Unlock()
	err := c.quicConn.CloseWithError(closeErrorCodeGeneric, "")
	c.closed = true
	return err
}

func (c *Client) quicPacketConn() (net.PacketConn, error) {
	pktConn, _ := dialer.ListenPacket(c.context, "udp", "", []dialer.Option{}...)
	udpConn, _ := pktConn.(*net.UDPConn)
	log.Infoln("hysteria: udpConn %p", udpConn)

	if len(c.protocol) == 0 || c.protocol == "udp" {
		if c.obfuscator != nil {
			pktConn = udp.NewObfsUDPConn(udpConn, c.obfuscator)
		}
	} else if c.protocol == "wechat-video" {
		pktConn = wechat.NewObfsWeChatUDPConn(udpConn, c.obfuscator)
	} else {
		return nil, fmt.Errorf("unsupported protocol: %s", c.protocol)
	}

	return pktConn, nil
}

func (c *Client) QUICDial() (quic.Connection, error) {
	log.Infoln("hysteria: quic dial")
	serverUDPAddr, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return nil, err
	}
	pktConn, err := c.quicPacketConn()
	if err != nil {
		return nil, err
	}
	quicConn, err := quic.Dial(pktConn, serverUDPAddr, c.serverAddr, c.tlsConfig, c.quicConfig)
	if err != nil {
		log.Errorln("hysteria: quic dial failed %v", err)
		// log.Errorln("hysteria: failure stack: %s", debug.Stack())
		_ = pktConn.Close()
		return nil, err
	}
	return quicConn, nil
}
