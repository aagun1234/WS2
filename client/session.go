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
	clientConn net.Conn
	tunnel     *Tunnel
	sessions   *sync.Map // Reference back to the manager's sessions map
	closed     chan struct{}
	once       sync.Once
}

// NewSession creates a new session.
func NewSession(sessionID uint64, clientConn net.Conn, tunnel *Tunnel, sessions *sync.Map) *Session {
	return &Session{
		SessionID:  sessionID,
		clientConn: clientConn,
		tunnel:     tunnel,
		sessions:   sessions,
		closed:     make(chan struct{}),
	}
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
					if err := s.tunnel.WriteMessage(protocol.NewDataMessage(s.SessionID, buf[:n])); err != nil {
						log.Printf("[Session %d] Error sending data over tunnel: %v", s.SessionID, err)
						s.Close()
						return
					}
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