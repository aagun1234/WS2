package client

import (
	//"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
	"github.com/aagun1234/ws2/protocol"
)

// Session manages a single client connection and its corresponding stream over the tunnel.
type Session struct {
	SessionID  uint64
	nextSequenceID uint64
	sendSequenceID uint64
	clientConn net.Conn
	tunnel     *Tunnel
	sessions   *sync.Map // Reference back to the manager's sessions map
	closed     chan struct{}
	once       sync.Once

	rxmu             sync.RWMutex
	receiveBuffer  map[uint64]*protocol.TunnelMessage // 存储乱序到达的消息
	bufferCond     *sync.Cond                // 用于通知等待新消息的消费者
	
	
}

// NewSession creates a new session.
func NewSession(sessionID uint64, clientConn net.Conn, tunnel *Tunnel, sessions *sync.Map) *Session {
	sess := &Session{
		SessionID:  sessionID,
		clientConn: clientConn,
		tunnel:     tunnel,
		sessions:   sessions,
		closed:     make(chan struct{}),
		receiveBuffer: make(map[uint64]*protocol.TunnelMessage),
	}
	sess.sendSequenceID=0
	sess.bufferCond = sync.NewCond(&sess.rxmu) // 使用 session 的 RWMutex 作为 Cond 的 Locker
	return sess
}



// StartMessageProcessing 启动一个 goroutine 来处理会话接收到的消息 remote->local
func (s *Session) StartMessage() { 
	go func() {
		for {
			s.rxmu.Lock()
			for { //循环等待直到有nextSequenceID
				msg, exists := s.receiveBuffer[s.nextSequenceID]
				if exists {
					if msg.SessionID==s.SessionID {
						// 找到了期待的消息，可以处理了
						delete(s.receiveBuffer, s.nextSequenceID) // 从缓冲区中移除
						s.nextSequenceID++                       // 更新期待的下一条序列号
						s.rxmu.Unlock()

						// 处理并转发消息
						if _, err := s.clientConn.Write(msg.Payload); err != nil {
							log.Printf("[SessionProcess] [Session %d] Error writing data to client connection: %v", s.SessionID, err)
							s.Close() // Close session if client write fails
							return    // 退出处理循环
						}
						break // 跳出内层循环，继续处理下一条消息
					} else {
						log.Printf("[SessionProcess] [Session %d] SessionID MisMatch Error, %d != %d", s.SessionID, s.SessionID, msg.SessionID )
					}
				} else {
					// 如果缓冲区中没有期待的消息，则等待
					s.bufferCond.Wait() // 释放锁并等待 Signal
					// Signal 收到后，Wait 会重新获取锁并返回，然后再次检查条件
				}
			}

			// 检查会话是否已关闭
			select {
			case <-s.closed:

				return // 会话关闭，退出 goroutine
			default:
				// 继续处理
			}
		}
	}()
	// Keep this goroutine alive until session is closed (waits on s.closed)
	<-s.closed
}


// Start initiates the data forwarding for the session.
func (s *Session) Start() {
	defer s.Close() // Ensure session is closed when goroutine exits

	// Goroutine to read from client and send over tunnel
	go func() {
		buf := make([]byte, protocol.MaxPayloadSize) // Use MaxPayloadSize for buffer
		for {
			select {
			case <-s.closed:
				return
			default:
				// Read from client with a deadline to make the loop responsive to s.closed
				s.clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := s.clientConn.Read(buf)
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue // Timeout, recheck closed status
					}
					if err != io.EOF {
						log.Printf("[Session %d] Error reading from client connection: %v", s.SessionID, err)
					} else {
						log.Printf("[Session %d] Client disconnected (EOF).", s.SessionID)
					}
					s.Close()
					return
				}

				if n > 0 {
					// Send data over tunnel
					log.Printf("[Session %d] select tunnel %d , Seq:%d", s.SessionID, s.tunnel.ID, s.sendSequenceID)
					if err := s.tunnel.WriteMessage(protocol.NewDataMessage(s.SessionID, s.sendSequenceID, buf[:n])); err != nil {
						log.Printf("[Session %d] Error sending data over tunnel: %v", s.SessionID, err)
						s.Close()
						return
					}
					s.sendSequenceID++
				}
			}
		}
	}()

	// Keep this goroutine alive until session is closed (waits on s.closed)
	<-s.closed
}

// Close closes the session and cleans up resources.
func (s *Session) Close() {
	s.once.Do(func() {
		log.Printf("[Session %d] Closing session.", s.SessionID)
		close(s.closed)

		// Remove from manager's map
		s.sessions.Delete(s.SessionID)

		// Close client connection
		if s.clientConn != nil {
			s.clientConn.Close()
		}

		// Send close message to server via the tunnel (if tunnel is still connected)
		if s.tunnel != nil && s.tunnel.IsConnected() { // Ensure tunnel is still healthy before writing
			if err := s.tunnel.WriteMessage(protocol.NewCloseSessionMessage(s.SessionID)); err != nil {
				log.Printf("[Session %d] Error sending close message over tunnel: %v", s.SessionID, err)
			}
		}
	})
}