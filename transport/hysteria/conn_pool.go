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
	packetQueueSize   = 256
	recvBufferSize    = 1500
	routineMaxSend    = 1024 * 32
	defaultConcurrent = 2
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

	if cp.concurrent <= 0 {
		cp.concurrent = defaultConcurrent
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

	addrPool   []net.Addr
	workPool   map[*connRoutine]interface{}
	idxPool    map[int]interface{}
	history    []*connRoutine
	rotateLock sync.Mutex

	recvQueue chan *udpPacket

	closed bool
}

func (cp *connPool) Init() {
	cp.recvQueue = make(chan *udpPacket, packetQueueSize)
	randIndices := rand.Perm(len(cp.addrPool))[:cp.concurrent]
	for i := 0; i < cp.concurrent; i++ {
		r := cp.newConnRoutine(randIndices[i])
		cp.history = append(cp.history, r)
	}
}

func (cp *connPool) getPacketConn() (net.PacketConn, error) {
	return GetPacketConn(*cp.context, cp.protocol, cp.obfuscator)
}

func (cp *connPool) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	if cp.closed {
		return 0, nil, ErrConnClosed
	}
	addr = cp.serverAddr

	p := <-cp.recvQueue
	if p == nil {
		// comment below line out due to QUIC conn reuse would fail
		// so we just swallow the error and return nothing
		// err = ErrConnClosed
		return
	}
	n = copy(b, p.buf.Bytes()[:p.n])
	bytebufferpool.Put(p.buf)
	return
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
			var r *connRoutine
			r, err = cp.rotate(conn)
			if err == nil {
				n, err = r.writeTo(p)
			} else if err == ErrConnRotated {
				continue
			}
			return
		}
		log.Errorln("hysteria: concurrent WriteTo error: %v", err)
	}
	return
}

func (cp *connPool) Close() error {
	cp.closed = true
	log.Infoln("hyeteria: close pool")
	// Grace time before closing pool connections
	time.Sleep(500 * time.Millisecond)
	for _, conn := range cp.history {
		conn.close()
	}
	// Grace time before closing receive queue
	time.Sleep(500 * time.Millisecond)
	close(cp.recvQueue)
	return nil
}

func (cp *connPool) LocalAddr() net.Addr {
	return cp.history[len(cp.history)-1].conn.LocalAddr()
}

func (cp *connPool) SetDeadline(t time.Time) error {
	log.Infoln("hysteria: SetDeadline %d", t.Second())
	return nil
}

func (cp *connPool) SetReadDeadline(t time.Time) error {
	log.Infoln("hysteria: SetReadDeadline %d", t.Second())
	return nil
}

func (cp *connPool) SetWriteDeadline(t time.Time) error {
	log.Infoln("hysteria: SetWriteDeadline %d", t.Second())
	return nil
}

func (cp *connPool) SetReadBuffer(bytes int) error {
	log.Infoln("hysteria: read buffer: %d", bytes)
	return nil
}

func (cp *connPool) SetWriteBuffer(bytes int) error {
	log.Infoln("hysteria: write buffer: %d", bytes)
	return nil
}

func (cp *connPool) SyscallConn() (syscall.RawConn, error) {
	return &RawConn{cp.workPool}, nil
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

func (cp *connPool) rotate(old *connRoutine) (*connRoutine, error) {
	if cp.closed {
		return nil, ErrConnClosed
	}

	cp.rotateLock.Lock()
	defer cp.rotateLock.Unlock()
	if _, found := cp.workPool[old]; !found {
		return nil, ErrConnRotated
	}

	var i int
	for {
		i = rand.Intn(len(cp.addrPool))
		if _, found := cp.idxPool[i]; !found {
			break
		}
	}
	newConn := cp.newConnRoutine(i)

	// remove max send connRoutine from work pool
	log.Infoln("hysteria: recycle conn %s", old.remoteAddr)
	delete(cp.workPool, old)
	delete(cp.idxPool, old.addrPoolIdx)

	cp.history = append(cp.history, newConn)
	if len(cp.history) >= 4*cp.concurrent {
		used := cp.history[:cp.concurrent]
		cp.history = cp.history[cp.concurrent:]
		for _, conn := range used {
			log.Infoln("hysteria: close conn %s", conn.remoteAddr)
			conn.close()
		}
	}

	return newConn, nil
}

func (cp *connPool) newConnRoutine(idx int) *connRoutine {
	pktConn, _ := cp.getPacketConn()

	r := &connRoutine{
		addrPoolIdx:   idx,
		conn:          pktConn,
		poolRecvQueue: cp.recvQueue,
		remoteAddr:    cp.addrPool[idx],
		maxSend:       uint32(rand.Intn(routineMaxSend)) + routineMaxSend, // approx 32-64MB for this connRoutine
	}
	go r.recvLoop()
	log.Infoln("hysteria: new conn %v, maxsend: %v", r.remoteAddr, r.maxSend)
	cp.workPool[r] = struct{}{}
	cp.idxPool[idx] = struct{}{}
	return r
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
			if nErr, ok := err.(net.Error); ok && nErr.Temporary() {
				continue
			}
			c.sendCounter.Store(3 * routineMaxSend)
			break
		}
		select {
		case c.poolRecvQueue <- &udpPacket{poolBuf, n, addr}:
		default:
			// Drop the packet if the queue is full
			log.Errorln("hysteria: connRoutine dropped %d bytes", n)
			bytebufferpool.Put(poolBuf)
		}
	}
}

func (c *connRoutine) writeTo(p []byte) (int, error) {
	sendCount := c.sendCounter.Add(1)
	if sendCount > c.maxSend {
		log.Infoln("hysteria: reached max %s", c.remoteAddr)
		return 0, ErrMaxSend
	}
	return c.conn.WriteTo(p, c.remoteAddr)
}

type RawConn struct {
	workPool map[*connRoutine]interface{}
}

func (r *RawConn) Control(f func(fd uintptr)) error {
	log.Debugln("hysteria: SyscallConn Control")
	for c := range r.workPool {
		sc, _ := c.conn.(syscall.Conn)
		raw, _ := sc.SyscallConn()
		raw.Control(f)
	}
	return nil
}

func (r *RawConn) Read(f func(fd uintptr) (done bool)) error {
	log.Debugln("hysteria: SyscallConn Read")
	for c := range r.workPool {
		sc, _ := c.conn.(syscall.Conn)
		raw, _ := sc.SyscallConn()
		raw.Read(f)
	}
	return nil
}

func (r *RawConn) Write(f func(fd uintptr) (done bool)) error {
	log.Debugln("hysteria: SyscallConn Write")
	for c := range r.workPool {
		sc, _ := c.conn.(syscall.Conn)
		raw, _ := sc.SyscallConn()
		raw.Write(f)
	}
	return nil
}
