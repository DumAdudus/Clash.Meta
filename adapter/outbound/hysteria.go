package outbound

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/Dreamacro/clash/component/dialer"
	tlsC "github.com/Dreamacro/clash/component/tls"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"
	"github.com/Dreamacro/clash/transport/hysteria"

	"github.com/HyNetwork/hysteria/pkg/pmtud"
	"github.com/HyNetwork/hysteria/pkg/transport/pktconns/obfs"
	"github.com/lucas-clemente/quic-go"
)

const (
	mbpsToBps   = 125000
	minSpeedBPS = 16384

	DefaultStreamReceiveWindow     = 15728640 // 15 MB/s
	DefaultConnectionReceiveWindow = 67108864 // 64 MB/s
	DefaultMaxIncomingStreams      = 1024

	DefaultALPN     = "hysteria"
	DefaultProtocol = "udp"
)

var rateStringRegexp = regexp.MustCompile(`^(\d+)\s*([KMGT]?)([Bb])ps$`)

type Hysteria struct {
	*Base

	client *hysteria.Client
}

func (h *Hysteria) baseConnSetup(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) error {
	pktConn, _ := dialer.ListenPacket(ctx, "udp", "", h.Base.DialOptions(opts...)...)
	udpconn, _ := pktConn.(*net.UDPConn)
	h.client.UDPSetup(ctx, udpconn)
	return nil
}

func (h *Hysteria) DialContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (C.Conn, error) {
	h.baseConnSetup(ctx, metadata, opts...)
	tcpConn, err := h.client.DialTCP(ctx, metadata.RemoteAddress())
	if err != nil {
		return nil, err
	}

	return NewConn(tcpConn, h), nil
}

func (h *Hysteria) ListenPacketContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (C.PacketConn, error) {
	h.baseConnSetup(ctx, metadata, opts...)
	udpConn, err := h.client.DialUDP()
	if err != nil {
		return nil, err
	}
	return newPacketConn(udpConn, h), nil
}

type HysteriaOption struct {
	BasicOption
	Name                string `proxy:"name"`
	Server              string `proxy:"server"`
	Port                int    `proxy:"port"`
	Protocol            string `proxy:"protocol,omitempty"`
	Up                  string `proxy:"up"`
	Down                string `proxy:"down"`
	AuthString          string `proxy:"auth_str,omitempty"`
	Obfs                string `proxy:"obfs,omitempty"`
	SNI                 string `proxy:"sni,omitempty"`
	SkipCertVerify      bool   `proxy:"skip-cert-verify,omitempty"`
	Fingerprint         string `proxy:"fingerprint,omitempty"`
	ALPN                string `proxy:"alpn,omitempty"`
	CustomCA            string `proxy:"ca,omitempty"`
	CustomCAString      string `proxy:"ca_str,omitempty"`
	ReceiveWindowConn   int    `proxy:"recv_window_conn,omitempty"`
	ReceiveWindow       int    `proxy:"recv_window,omitempty"`
	DisableMTUDiscovery bool   `proxy:"disable_mtu_discovery,omitempty"`
}

func (c *HysteriaOption) Speed() (uint64, uint64, error) {
	up := stringToBps(c.Up)
	if up == 0 {
		return 0, 0, fmt.Errorf("invaild upload speed: %s", c.Up)
	}

	down := stringToBps(c.Down)
	if down == 0 {
		return 0, 0, fmt.Errorf("invaild download speed: %s", c.Down)
	}

	return up, down, nil
}

func NewHysteria(option HysteriaOption) (*Hysteria, error) {
	addr := net.JoinHostPort(option.Server, strconv.Itoa(option.Port))
	serverName := option.Server
	if option.SNI != "" {
		serverName = option.SNI
	}

	tlsConfig := &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: option.SkipCertVerify,
		MinVersion:         tls.VersionTLS13,
	}

	var bs []byte
	var err error
	if len(option.CustomCA) > 0 {
		bs, err = ioutil.ReadFile(option.CustomCA)
		if err != nil {
			return nil, fmt.Errorf("hysteria %s load ca error: %w", addr, err)
		}
	} else if option.CustomCAString != "" {
		bs = []byte(option.CustomCAString)
	}

	if len(bs) > 0 {
		block, _ := pem.Decode(bs)
		if block == nil {
			return nil, fmt.Errorf("CA cert is not PEM")
		}

		fpBytes := sha256.Sum256(block.Bytes)
		if len(option.Fingerprint) == 0 {
			option.Fingerprint = hex.EncodeToString(fpBytes[:])
		}
	}

	if len(option.Fingerprint) != 0 {
		var err error
		tlsConfig, err = tlsC.GetSpecifiedFingerprintTLSConfig(tlsConfig, option.Fingerprint)
		if err != nil {
			return nil, err
		}
	} else {
		tlsConfig = tlsC.GetGlobalFingerprintTLCConfig(tlsConfig)
	}

	if len(option.ALPN) > 0 {
		tlsConfig.NextProtos = []string{option.ALPN}
	} else {
		tlsConfig.NextProtos = []string{DefaultALPN}
	}

	quicConfig := &quic.Config{
		InitialStreamReceiveWindow:     uint64(option.ReceiveWindowConn),
		MaxStreamReceiveWindow:         uint64(option.ReceiveWindowConn),
		InitialConnectionReceiveWindow: uint64(option.ReceiveWindow),
		MaxConnectionReceiveWindow:     uint64(option.ReceiveWindow),
		KeepAlivePeriod:                10 * time.Second,
		DisablePathMTUDiscovery:        option.DisableMTUDiscovery,
		EnableDatagrams:                true,
	}
	if option.Protocol == "" {
		option.Protocol = DefaultProtocol
	}
	if option.ReceiveWindowConn == 0 {
		quicConfig.InitialStreamReceiveWindow = DefaultStreamReceiveWindow
		quicConfig.MaxStreamReceiveWindow = DefaultStreamReceiveWindow
	}
	if option.ReceiveWindow == 0 {
		quicConfig.InitialConnectionReceiveWindow = DefaultConnectionReceiveWindow
		quicConfig.MaxConnectionReceiveWindow = DefaultConnectionReceiveWindow
	}
	if !quicConfig.DisablePathMTUDiscovery && pmtud.DisablePathMTUDiscovery {
		log.Infoln("hysteria: Path MTU Discovery is not yet supported on this platform")
	}

	auth := []byte(option.AuthString)
	var obfuscator obfs.Obfuscator
	if len(option.Obfs) > 0 {
		obfuscator = obfs.NewXPlusObfuscator([]byte(option.Obfs))
	}

	up, down, err := option.Speed()
	if err != nil {
		return nil, err
	}

	client, err := hysteria.NewClient(
		addr, option.Protocol, auth, tlsConfig, quicConfig, up, down, obfuscator,
	)
	if err != nil {
		return nil, fmt.Errorf("hysteria %s create error: %w", addr, err)
	}
	return &Hysteria{
		Base: &Base{
			name:  option.Name,
			addr:  addr,
			tp:    C.Hysteria,
			udp:   true,
			iface: option.Interface,
			rmark: option.RoutingMark,
		},
		client: client,
	}, nil
}

func stringToBps(s string) uint64 {
	if s == "" {
		return 0
	}

	// when have no unit, use Mbps
	if v, err := strconv.Atoi(s); err == nil {
		return stringToBps(fmt.Sprintf("%d Mbps", v))
	}

	m := rateStringRegexp.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	var n uint64
	switch m[2] {
	case "K":
		n = 1 << 10
	case "M":
		n = 1 << 20
	case "G":
		n = 1 << 30
	case "T":
		n = 1 << 40
	default:
		n = 1
	}
	v, _ := strconv.ParseUint(m[1], 10, 64)
	n = v * n
	if m[3] == "b" {
		// Bits, need to convert to bytes
		n = n >> 3
	}
	return n
}
