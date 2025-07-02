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
	wsConn    *WebSocketConn // Reference back to the specific WebSocket connection
	targetConn net.Conn      // The actual connection to the target
	manager   *SessionManager // Reference back to the manager
	closed    chan struct{}
	once      sync.Once
}

// NewTargetSession creates a new TargetSession.
func NewTargetSession(sessionID uint64, wsConn *WebSocketConn, manager *SessionManager, targetConn net.Conn) *TargetSession {
	return &TargetSession{
		SessionID:  sessionID,
		wsConn:    wsConn,
		targetConn: targetConn,
		manager:   manager,
		closed:    make(chan struct{}),
	}
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
					//log.Printf("[Server Session %d] Received %d bytes of data from target connection %s, sending to %s", ts.SessionID,n, ts.targetConn.RemoteAddr(),ts.wsConn.conn.UnderlyingConn().RemoteAddr())
					if err := ts.wsConn.WriteMessage(protocol.NewDataMessage(ts.SessionID, buf[:n])); err != nil {
						log.Printf("[Server Session %d] Error sending data over WebSocket: %v", ts.SessionID, err)
						ts.Close()
						return
					}
					//log.Printf("[Server Session %d] Data Sent (%d bytes)", ts.SessionID,n)
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