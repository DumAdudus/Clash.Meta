package hysteria

import (
	"bytes"
	"sync"

	"github.com/Dreamacro/clash/log"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
)

const chanBuffer = 8

type udpSession struct {
	sessionMap map[uint32]chan *udpMessage
	mapRWLock  sync.RWMutex
	defragger  defragger
}

func NewUdpSession() *udpSession {
	s := &udpSession{}
	s.sessionMap = make(map[uint32]chan *udpMessage)
	return s
}

func (s *udpSession) CreateSession(sessionId uint32) <-chan *udpMessage {
	nCh := make(chan *udpMessage, chanBuffer)

	s.mapRWLock.Lock()
	s.sessionMap[sessionId] = nCh
	s.mapRWLock.Unlock()

	return nCh
}

func (s *udpSession) CloseSession(sessionId uint32) {
	s.mapRWLock.Lock()
	defer s.mapRWLock.Unlock()

	if ch, ok := s.sessionMap[sessionId]; ok {
		close(ch)
		delete(s.sessionMap, sessionId)
	}
}

func (s *udpSession) handleMessage(conn quic.Connection) {
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
		dfMsg := s.defragger.Feed(udpMsg)
		if dfMsg == nil {
			continue
		}

		s.mapRWLock.RLock()
		log.Debugln("hysteria: handleMessage %v, %v bytes", dfMsg.MsgID, dfMsg.DataLen)
		ch, ok := s.sessionMap[dfMsg.SessionID]
		s.mapRWLock.RUnlock()

		if ok {
			select {
			case ch <- dfMsg:
				// OK
			default:
				log.Errorln("hysteria: udpSession dropped %v bytes", dfMsg.DataLen)
				// Silently drop the message when the channel is full
			}
		}
	}
}
