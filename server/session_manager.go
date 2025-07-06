package server

import (
	//"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time" 
	"github.com/aagun1234/ws2/protocol"
)

// TargetSession represents an outbound connection to a target host on the server side.
type TargetSession struct {
	SessionID uint64
	nextSequenceID uint64
	sendSequenceID uint64
	wsConn    *WebSocketConn // Reference back to the specific WebSocket connection
	targetConn net.Conn      // The actual connection to the target
	manager   *SessionManager // Reference back to the manager
	closed    chan struct{}
	once      sync.Once
	
	rxmu             sync.RWMutex
	receiveBuffer  map[uint64]*protocol.TunnelMessage // 存储乱序到达的消息
	bufferCond     *sync.Cond                // 用于通知等待新消息的消费者
	
}

// NewTargetSession creates a new TargetSession.
func NewTargetSession(sessionID uint64, wsConn *WebSocketConn, manager *SessionManager, targetConn net.Conn) *TargetSession {
	var tsess *TargetSession

		tsess = &TargetSession{
			SessionID:  sessionID,
			sendSequenceID: 0,
			nextSequenceID: 0,
			//wsConns:    wsConns,
			wsConn:    wsConn,
			targetConn: targetConn,
			manager:   manager,
			closed:    make(chan struct{}),
			receiveBuffer: make(map[uint64]*protocol.TunnelMessage),
		}
		tsess.bufferCond = sync.NewCond(&tsess.rxmu) // 使用 session 的 RWMutex 作为 Cond 的 Locker
	return tsess
}




// StartMessageProcessing 启动一个 goroutine 来处理会话接收到的消息 remote->local
func (ts *TargetSession) StartMessage() { 
	go func() {
		for {
			log.Printf("[Server] [Message Buffer Processing], waiting for %d, Bufferlen: %d", ts.nextSequenceID,len(ts.receiveBuffer))
			ts.rxmu.Lock()
			for { //循环等待直到有nextSequenceID
				msg, exists := ts.receiveBuffer[ts.nextSequenceID]
				if exists {
					log.Printf("[Server] [Message Buffer Processing] Message with seqID:%d, SessionID: %d, PayloadLen:%d",ts.nextSequenceID, msg.SessionID, len(msg.Payload))
					if msg.SessionID==ts.SessionID && msg.SequenceID==ts.nextSequenceID {
						// 找到了期待的消息，可以处理了
						
						delete(ts.receiveBuffer, ts.nextSequenceID) // 从缓冲区中移除
						log.Printf("[Server] [Message Buffer Processing] Message delete from buffer, bufferlen: %d",len(ts.receiveBuffer))
						ts.nextSequenceID++                       // 更新期待的下一条序列号
						ts.rxmu.Unlock()

						// 处理并转发消息
						if _, err := ts.targetConn.Write(msg.Payload); err != nil {
							log.Printf("[Server] [Message Buffer Processing] WS Error writing data to target for session %d: %v",  msg.SessionID, err)
							ts.Close() // Close session if client write fails
							return    // 退出处理循环
						} else {
							log.Printf("[Server] [Message Buffer Processing] session %d: Message sent to %s",  msg.SessionID, ts.targetConn.RemoteAddr())
						}
						
						break // 跳出内层循环，继续处理下一条消息
					} else {
						
						delete(ts.receiveBuffer, ts.nextSequenceID) // 从缓冲区中移除
						log.Printf("[Server] [Message Buffer Processing] WS Message %d SessionID MisMatch Error, %d != %d, just remove from Buffer, bufferlen:%d",  msg.SequenceID, msg.SessionID, ts.SessionID, len(ts.receiveBuffer))
					}
					
				} else {
					// 如果缓冲区中没有期待的消息，则等待
					ts.bufferCond.Wait() // 释放锁并等待 Signal
					// Signal 收到后，Wait 会重新获取锁并返回，然后再次检查条件
				}
			}

			// 检查会话是否已关闭
			select {
			case <-ts.closed:

				return // 会话关闭，退出 goroutine
			default:
				// 继续处理
			}
		}
	}()
	// Keep this goroutine alive until session is closed (waits on ts.closed)
	<-ts.closed
}





// Start initiates the data forwarding for the target session.
func (ts *TargetSession) Start() {
	defer ts.Close() // Ensure session is closed when goroutine exits

	// Goroutine to read from target and send over WebSocket
	go func() {
		buf := make([]byte, protocol.MaxPayloadSize)
		for {
			select {
			case <-ts.closed:
				return
			default:
				// Read from target with a deadline to make the loop responsive to ts.closed
				ts.targetConn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := ts.targetConn.Read(buf)
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue // Timeout, recheck closed status
					}
					if err != io.EOF {
						log.Printf("[Server Session %d] Error reading from target connection: %v", ts.SessionID, err)
					} 
					ts.Close()
					return
				}

				if n > 0 {
					log.Printf("[Server Session %d] Received %d bytes of data from target connection %s, %v , %v", ts.SessionID, n, ts.targetConn.RemoteAddr(),buf[:n], (ts.wsConn==nil))
					msg:=protocol.NewDataMessage(ts.SessionID, ts.sendSequenceID, buf[:n])
					if ts.wsConn.IsConnected() {
						if err := ts.wsConn.WriteMessage(msg); err != nil {
							log.Printf("[Server Session %d] Error sending data (Seq:%d) over WebSocket: %v", ts.SessionID, ts.sendSequenceID, err)
							ts.Close()
							return
						}
					} else {
						log.Printf("[Server Session %d] WebSocket NOT CONNECTED", ts.SessionID, ts.sendSequenceID)
					}
					
					log.Printf("[Server Session %d] %d bytes Data Sent (SeqID：%d)", ts.SessionID, n, ts.sendSequenceID)
					ts.sendSequenceID++
					
				}
			}
		}
	}()

	// Keep this goroutine alive until session is closed (waits on ts.closed)
	<-ts.closed
}

// Close closes the target session and cleans up resources.
func (ts *TargetSession) Close() {
	ts.once.Do(func() {
		//log.Printf("[Server Session %d] Closing target session.", ts.SessionID)
		close(ts.closed)

		// Remove from manager's map
		ts.manager.DeleteSession(ts.SessionID)

		// Close target connection
		if ts.targetConn != nil {
			ts.targetConn.Close()
		}

		// Send close message back to client via WebSocket (if WS connection is still healthy)
		if ts.wsConn != nil && ts.wsConn.IsConnected() { // Ensure WebSocket is still healthy before writing
			if err := ts.wsConn.WriteMessage(protocol.NewCloseSessionMessage(ts.SessionID)); err != nil {
				log.Printf("[Server Session %d] Error sending close message back to client: %v", ts.SessionID, err)
			}
		}
	})
}

// SessionManager manages active target sessions on the server side.
type SessionManager struct {
	sessions *sync.Map // map[uint64]*TargetSession
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: &sync.Map{},
	}
}

// StoreSession adds a new session to the manager.
func (sm *SessionManager) StoreSession(sessionID uint64, sess *TargetSession) {
	sm.sessions.Store(sessionID, sess)
}

// LoadSession retrieves a session from the manager.
func (sm *SessionManager) LoadSession(sessionID uint64) (*TargetSession, bool) {
	val, ok := sm.sessions.Load(sessionID)
	if !ok {
		return nil, false
	}
	return val.(*TargetSession), true
}

// DeleteSession removes a session from the manager.
func (sm *SessionManager) DeleteSession(sessionID uint64) {
	sm.sessions.Delete(sessionID)
}

// CloseAllSessionsForWebSocket closes all sessions associated with a specific WebSocket connection.
func (sm *SessionManager) CloseAllSessionsForWebSocket(wsConn *WebSocketConn) {
	sm.sessions.Range(func(key, value interface{}) bool {
		sessionID := key.(uint64)
		sess := value.(*TargetSession)
		if sess.wsConn == wsConn {
			log.Printf("[Server Session %d] Closing session due to WebSocket disconnection.", sessionID)
			sess.Close() // This will also delete it from the map
		}
		return true
	})
}