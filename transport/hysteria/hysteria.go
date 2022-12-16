// Modified from: https://github.com/apernet/hysteria
// License: MIT

package hysteria

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Dreamacro/clash/component/dialer"
	"github.com/Dreamacro/clash/log"

	"github.com/apernet/hysteria/core/congestion"
	"github.com/apernet/hysteria/core/pktconns/obfs"
	"github.com/apernet/hysteria/core/pktconns/udp"
	"github.com/apernet/hysteria/core/pktconns/wechat"
	"github.com/apernet/hysteria/core/pmtud"
	"github.com/apernet/hysteria/core/utils"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
)

var serverConnInterval = 5 * time.Second

type Client struct {
	dialContext      context.Context
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
	lastConnTime   time.Time
	udpSession     *udpSession
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
	c.reconnectMutex.Lock()
	defer c.reconnectMutex.Unlock()

	// We have no effective way to verify if c.quicConn is working
	// To deal with concurrent connections, we will allow only one
	// try of connectToServer within 5 seconds
	if c.lastConnTime.Add(serverConnInterval).After(time.Now()) {
		return nil
	}

	// Clear previous connection
	if c.quicConn != nil {
		_ = c.quicConn.CloseWithError(0, "")
	}

	log.Infoln("hysteria: client %p connect to server: %s", c, c.serverAddr)

	quicConn, err := c.getQuicConn()
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
	c.udpSession = NewUdpSession()
	go c.udpSession.handleMessage(quicConn)
	c.quicConn = quicConn
	c.lastConnTime = time.Now()
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

func (c *Client) openStream() (quic.Stream, error) {
	if c.quicConn == nil {
		return nil, errors.New("no_conn")
	}
	if c.closed {
		return nil, errors.New("closed")
	}

	stream, err := c.quicConn.OpenStream()
	if err != nil {
		// We will tolerate temporary network error
		if nErr, ok := err.(net.Error); !(ok && nErr.Temporary()) {
			return nil, err
		}
	}
	return &qStream{stream}, nil
}

func (c *Client) openStreamWithReconnect() (quic.Stream, error) {
	stream, err := c.openStream()
	if err != nil && err.Error() != "closed" {
		// need to reconnect
		if err := c.connectToServer(); err != nil {
			return nil, err
		}
		stream, err = c.openStream()
	}
	return stream, err
}

func (c *Client) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	log.Infoln("hysteria: dialtcp")
	c.dialContext = ctx
	host, port, err := utils.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	stream, err := c.openStreamWithReconnect()
	if err != nil {
		return nil, err
	}

	// Send request
	err = SendServerReq(stream, &clientRequest{
		UDP:  false,
		Host: host,
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	// If fast open is enabled, we return the stream immediately
	// and defer the response handling to the first Read() call
	if !c.fastOpen {
		// Read response
		var sr serverResponse
		err = HandleServerResp(stream, &sr)
		if err != nil {
			return nil, err
		}
	}

	return &quicStream{
		Stream:           stream,
		PseudoLocalAddr:  c.quicConn.LocalAddr(),
		PseudoRemoteAddr: c.quicConn.RemoteAddr(),
		Established:      !c.fastOpen,
	}, nil
}

func (c *Client) DialUDP(ctx context.Context) (net.PacketConn, error) {
	log.Infoln("hysteria: dialudp")
	c.dialContext = ctx
	stream, err := c.openStreamWithReconnect()
	if err != nil {
		return nil, err
	}

	// Send request
	err = SendServerReq(stream, &clientRequest{
		UDP: true,
	})
	if err != nil {
		return nil, err
	}

	// Read response
	var sr serverResponse
	err = HandleServerResp(stream, &sr)
	if err != nil {
		return nil, err
	}

	// log.Infoln("hysteria: create udp session")
	// defer log.Infoln("hysteria: DialUDP return")
	pktConn := &quicPktConn{
		Connection:   c.quicConn,
		Stream:       stream,
		UdpSession:   c.udpSession,
		UDPSessionID: sr.UDPSessionID,
		MsgCh:        c.udpSession.CreateSession(sr.UDPSessionID),
	}
	go pktConn.Hold()
	return pktConn, nil
}

func (c *Client) getPacketConn() (net.PacketConn, error) {
	pktConn, _ := dialer.ListenPacket(c.dialContext, "udp", "", []dialer.Option{}...)
	udpConn, _ := pktConn.(*net.UDPConn)

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

func (c *Client) getQuicConn() (quic.Connection, error) {
	serverUDPAddr, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return nil, err
	}
	pktConn, err := c.getPacketConn()
	if err != nil {
		return nil, err
	}
	quicConn, err := quic.Dial(pktConn, serverUDPAddr, c.serverAddr, c.tlsConfig, c.quicConfig)
	if err != nil {
		log.Errorln("hysteria: getQuicConn failed %v", err)
		// log.Errorln("hysteria: failure stack: %s", debug.Stack())
		_ = pktConn.Close()
		return nil, err
	}
	return quicConn, nil
}

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
