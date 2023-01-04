// Modified from: https://github.com/apernet/hysteria
// License: MIT

package hysteria

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Dreamacro/clash/log"

	"github.com/apernet/hysteria/core/congestion"
	"github.com/apernet/hysteria/core/pktconns/obfs"
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
	multiPath        string
	concurrent       int

	tlsConfig  *tls.Config
	quicConfig *quic.Config

	quicConn       quic.Connection
	pktConn        *connStub
	reconnectMutex sync.Mutex
	closed         bool
	fastOpen       bool
	lastConnTime   time.Time
	udpSession     *udpSession
}

func NewClient(serverAddr string, protocol string, auth []byte, tlsConfig *tls.Config,
	quicConfig *quic.Config, sendBPS uint64, recvBPS uint64, obfuscator obfs.Obfuscator,
	fastOpen bool, multiPath string, concurrent int,
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
		multiPath:  multiPath,
		concurrent: concurrent,
	}

	return c, nil
}

func (c *Client) connectToServer(force bool) error {
	// c.reconnectMutex.Lock()
	// defer c.reconnectMutex.Unlock()

	// We have no effective way to verify if c.quicConn is working
	// To deal with concurrent connections, we will allow only one
	// try of connectToServer within 5 seconds
	if !force && c.lastConnTime.Add(serverConnInterval).After(time.Now()) {
		return nil
	}

	// If QUIC connection is already set, reuse it,
	// and reset underlying UDPConn
	if c.quicConn != nil {
		if force {
			_ = c.quicConn.CloseWithError(0, "")
			_ = c.pktConn.Close()
		} else {
			old := c.pktConn.PacketConn
			serverUDPAddr, err := net.ResolveUDPAddr("udp", c.serverAddr)
			if err != nil {
				return err
			}
			stub, err := c.getConnStub(serverUDPAddr)
			if err != nil {
				return err
			}
			c.pktConn.PacketConn = stub.PacketConn
			c.lastConnTime = time.Now()
			log.Infoln("hysteria: client %p reset UDPConn to server: %s", c, c.serverAddr)
			old.Close()
			return nil
		}
	}

	log.Infoln("hysteria: client %p connect to server: %s", c, c.serverAddr)

	quicConn, pktConn, err := c.getQuicConn()
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
	c.pktConn = pktConn
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
		return nil, ErrNoConn
	}
	if c.closed {
		return nil, ErrConnClosed
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

func (c *Client) openStreamWithReconnect() (stream quic.Stream, err error) {
	stream, err = c.openStream()
	if err != nil && err != ErrConnClosed {
		c.reconnectMutex.Lock()
		defer c.reconnectMutex.Unlock()
		// need to reconnect
		// just reset underlying UDPConn
		if err = c.connectToServer(false); err != nil {
			return
		}
		stream, err = c.openStream()
		// if the quic conn is closed remotely, we need to restart handshake
		// if _, ok := err.(*quic.IdleTimeoutError); ok {
		if err != nil {
			if err = c.connectToServer(true); err != nil {
				return
			}
			stream, err = c.openStream()
		}
	}
	return
}

func (c *Client) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
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

	return &hyTCPConn{
		Stream:           stream,
		PseudoLocalAddr:  c.quicConn.LocalAddr(),
		PseudoRemoteAddr: c.quicConn.RemoteAddr(),
		Established:      !c.fastOpen,
	}, nil
}

func (c *Client) DialUDP(ctx context.Context) (net.PacketConn, error) {
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
	pktConn := &hyUDPConn{
		Connection:   c.quicConn,
		Stream:       stream,
		UdpSession:   c.udpSession,
		UDPSessionID: sr.UDPSessionID,
		MsgCh:        c.udpSession.CreateSession(sr.UDPSessionID),
	}
	go pktConn.Hold()
	return pktConn, nil
}

func (c *Client) getFromPool(addr net.Addr) (net.PacketConn, error) {
	connPool, _ := NewConnPool(&c.dialContext, addr, c.protocol, c.obfuscator, c.multiPath, c.concurrent)
	connPool.Init()
	return connPool, nil
}

func (c *Client) getQuicConn() (quic.Connection, *connStub, error) {
	serverUDPAddr, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return nil, nil, err
	}
	stub, err := c.getConnStub(serverUDPAddr)
	if err != nil {
		return nil, nil, err
	}
	quicConn, err := quic.Dial(stub, serverUDPAddr, c.serverAddr, c.tlsConfig, c.quicConfig)
	if err != nil {
		log.Errorln("hysteria: getQuicConn failed %v", err)
		_ = stub.Close()
		return nil, nil, err
	}
	return quicConn, stub, nil
}

func (c *Client) getConnStub(serverUDPAddr *net.UDPAddr) (stub *connStub, err error) {
	var pktConn net.PacketConn
	if len(c.multiPath) > 0 {
		pktConn, err = c.getFromPool(serverUDPAddr)
	} else {
		pktConn, err = GetPacketConn(c.dialContext, c.protocol, c.obfuscator)
	}
	if err != nil {
		return
	}
	stub = &connStub{pktConn}
	return
}

type connStub struct {
	net.PacketConn
}
