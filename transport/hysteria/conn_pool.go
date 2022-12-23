package hysteria

import (
	"context"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Dreamacro/clash/log"
	"github.com/apernet/hysteria/core/pktconns/obfs"
	"github.com/valyala/bytebufferpool"
)

const (
	packetQueueSize = 128
	recvBufferSize  = 1500
	routineMaxSend  = 1024 * 8 //~8MB for each UDP routine
	defaultPaths    = 2
)

var (
	_       net.PacketConn = &connPool{}
	hintBuf                = make([]byte, recvBufferSize)
)

func NewConnPool(ctx *context.Context, addr net.Addr, protocol string, obfs obfs.Obfuscator, portRange string, concurrent int) (*connPool, error) {
	cp := &connPool{
		context:    ctx,
		serverAddr: addr,
		protocol:   protocol,
		obfuscator: obfs,
		concurrent: concurrent,
		workPool:   make(map[*connRoutine]interface{}),
		idxPool:    make(map[int]interface{}),
	}

	if cp.concurrent == 0 {
		cp.concurrent = defaultPaths
	}

	pRange := strings.Split(portRange, "-")
	begin, _ := strconv.ParseUint(pRange[0], 10, 16)
	end, _ := strconv.ParseUint(pRange[1], 10, 16)
	udpAddr, _ := addr.(*net.UDPAddr)
	for i := begin; i <= end; i++ {
		cp.addrPool = append(cp.addrPool, &net.UDPAddr{
			IP:   udpAddr.IP,
			Port: int(i),
			Zone: udpAddr.Zone,
		})
	}

	return cp, nil
}

type connPool struct {
	context    *context.Context
	serverAddr net.Addr
	protocol   string
	obfuscator obfs.Obfuscator
	concurrent int

	addrPool []net.Addr
	workPool map[*connRoutine]interface{}
	idxPool  map[int]interface{}

	history []*connRoutine
	hisLock sync.Mutex

	recvQueue chan *udpPacket

	closed bool
}

func (cp *connPool) Init() {
	cp.recvQueue = make(chan *udpPacket, packetQueueSize)
	randIndices := rand.Perm(len(cp.addrPool))[:cp.concurrent]
	for i := 0; i < cp.concurrent; i++ {
		pktConn, _ := cp.getPacketConn()
		r := &connRoutine{
			addrPoolIdx:   randIndices[i],
			conn:          pktConn,
			poolRecvQueue: cp.recvQueue,
			remoteAddr:    cp.addrPool[randIndices[i]],
			maxSend:       routineMaxSend,
		}
		go r.recvLoop()
		log.Infoln("New conn: %s", r.remoteAddr)
		cp.workPool[r] = struct{}{}
		cp.idxPool[randIndices[i]] = struct{}{}
		cp.history = append(cp.history, r)
	}
}

func (cp *connPool) getPacketConn() (net.PacketConn, error) {
	return GetPacketConn(*cp.context, cp.protocol, cp.obfuscator)
}

func (cp *connPool) ReadFrom(b []byte) (int, net.Addr, error) {
	p := <-cp.recvQueue
	n := copy(b, p.buf.Bytes()[:p.n])
	bytebufferpool.Put(p.buf)
	// fmt.Println("ReadFrom")
	return n, cp.serverAddr, nil
}

func (cp *connPool) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if cp.closed {
		return 0, ErrConnClosed
	}

	for i := 0; i < cp.concurrent; i++ {
		conn := cp.pickConn()
		n, err = conn.writeTo(p)
		if err == nil {
			return
		} else if err == ErrMaxSend {
			r, _ := cp.rotate()
			n, err = r.writeTo(p)
			return
		}
		log.Infoln("Write To error: %v", err)
	}
	return
}

func (cp *connPool) Close() error {
	cp.closed = true
	for _, conn := range cp.history {
		conn.close()
	}
	close(cp.recvQueue)
	return nil
}

func (cp *connPool) LocalAddr() net.Addr {
	return cp.history[len(cp.history)-1].conn.LocalAddr()
}

func (cp *connPool) SetDeadline(t time.Time) error {
	log.Infoln("SetDeadline %d", t.Second())
	return nil
}

func (cp *connPool) SetReadDeadline(t time.Time) error {
	log.Infoln("SetReadDeadline %d", t.Second())
	return nil
}

func (cp *connPool) SetWriteDeadline(t time.Time) error {
	log.Infoln("SetWriteDeadline %d", t.Second())
	return nil
}

func (cp *connPool) SetReadBuffer(bytes int) error {
	log.Infoln("Read buffer: %d", bytes)
	return nil
}

func (cp *connPool) SetWriteBuffer(bytes int) error {
	log.Infoln("Write buffer: %d", bytes)
	return nil
}

func (cp *connPool) SyscallConn() (syscall.RawConn, error) {
	return &RawConn{workPool: cp.workPool}, nil
}

func (cp *connPool) pickConn() *connRoutine {
	pick := rand.Intn(cp.concurrent)
	i := 0
	for c := range cp.workPool {
		if i == pick {
			return c
		}
		i++
	}
	return nil
}

func (cp *connPool) rotate() (*connRoutine, error) {
	var i int
	for {
		i = rand.Intn(len(cp.addrPool))
		if _, found := cp.idxPool[i]; !found {
			break
		}
	}
	pktConn, _ := cp.getPacketConn()
	r := &connRoutine{
		conn:          pktConn,
		poolRecvQueue: cp.recvQueue,
		remoteAddr:    cp.addrPool[i],
		maxSend:       routineMaxSend,
	}
	go r.recvLoop()
	log.Infoln("New conn: %s", r.remoteAddr)
	cp.workPool[r] = struct{}{}
	cp.idxPool[i] = struct{}{}

	used := cp.history[:cp.concurrent]

	cp.hisLock.Lock()
	cp.history = append(cp.history, r)
	if len(cp.history) >= 3*cp.concurrent {
		cp.history = cp.history[cp.concurrent:]
		for _, conn := range used {
			log.Infoln("Close conn: %s", conn.remoteAddr)
			conn.close()
		}
	}
	cp.hisLock.Unlock()

	for conn := range cp.workPool {
		if conn.isMaxSend() {
			log.Infoln("Recycle conn: %s", conn.remoteAddr)
			delete(cp.workPool, conn)
			delete(cp.idxPool, conn.addrPoolIdx)
			break
		}
	}
	return r, nil
}

type udpPacket struct {
	buf  *bytebufferpool.ByteBuffer
	n    int
	addr net.Addr
}

type connRoutine struct {
	addrPoolIdx   int
	remoteAddr    net.Addr
	conn          net.PacketConn
	poolRecvQueue chan<- *udpPacket

	sendCounter atomic.Uint32
	maxSend     uint32
}

func (c *connRoutine) close() error {
	return c.conn.Close()
}

func (c *connRoutine) recvLoop() {
	for {
		poolBuf := bytebufferpool.Get()
		poolBuf.Set(hintBuf)
		n, addr, err := c.conn.ReadFrom(poolBuf.Bytes())
		if err != nil {
			break
		}
		select {
		case c.poolRecvQueue <- &udpPacket{poolBuf, n, addr}:
		default:
			// Drop the packet if the queue is full
			log.Infoln("Dropped %d bytes", n)
			bytebufferpool.Put(poolBuf)
		}
	}
}

func (c *connRoutine) writeTo(p []byte) (int, error) {
	sendCount := c.sendCounter.Add(1)
	if sendCount > c.maxSend {
		log.Infoln("Reached max: %s", c.remoteAddr)
		return 0, ErrMaxSend
	}
	return c.conn.WriteTo(p, c.remoteAddr)
}

func (c *connRoutine) isMaxSend() bool {
	return c.sendCounter.Load() > c.maxSend
}

type RawConn struct {
	workPool map[*connRoutine]interface{}
}

func (r *RawConn) Control(f func(fd uintptr)) error {
	log.Infoln("SyscallConn Control")
	for c := range r.workPool {
		sc, _ := c.conn.(syscall.Conn)
		raw, _ := sc.SyscallConn()
		raw.Control(f)
	}
	return nil
}

func (r *RawConn) Read(f func(fd uintptr) (done bool)) error {
	log.Infoln("SyscallConn Read")
	for c := range r.workPool {
		sc, _ := c.conn.(syscall.Conn)
		raw, _ := sc.SyscallConn()
		raw.Read(f)
	}
	return nil
}

func (r *RawConn) Write(f func(fd uintptr) (done bool)) error {
	log.Infoln("SyscallConn Write")
	for c := range r.workPool {
		sc, _ := c.conn.(syscall.Conn)
		raw, _ := sc.SyscallConn()
		raw.Write(f)
	}
	return nil
}
