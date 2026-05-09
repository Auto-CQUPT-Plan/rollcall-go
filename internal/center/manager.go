package center

import (
	"log/slog"
	"sync"

	"github.com/gorilla/websocket"
)

// connEntry wraps a websocket connection with a per-connection write mutex.
// gorilla/websocket supports only one concurrent writer, so all writes
// (direct replies and broadcasts) must be serialized per connection.
type connEntry struct {
	conn     *websocket.Conn
	clientID string
	writeMu  sync.Mutex
}

type ConnectionManager struct {
	mu          sync.RWMutex
	connections map[*websocket.Conn]*connEntry
	log         *slog.Logger
}

func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		connections: make(map[*websocket.Conn]*connEntry),
		log:         slog.With("component", "conn_manager"),
	}
}

func (m *ConnectionManager) Connect(conn *websocket.Conn, clientID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connections[conn] = &connEntry{conn: conn, clientID: clientID}
	m.log.Info("Client connected", "client_id", clientID, "total", len(m.connections))
}

func (m *ConnectionManager) Disconnect(conn *websocket.Conn) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.connections[conn]
	clientID := ""
	if entry != nil {
		clientID = entry.clientID
	}
	delete(m.connections, conn)
	m.log.Info("Client disconnected", "client_id", clientID, "total", len(m.connections))
	return clientID
}

// WriteJSON sends a JSON message to a specific connection, serialized via per-conn mutex.
func (m *ConnectionManager) WriteJSON(conn *websocket.Conn, msg interface{}) error {
	m.mu.RLock()
	entry := m.connections[conn]
	m.mu.RUnlock()

	if entry == nil {
		return nil
	}

	entry.writeMu.Lock()
	defer entry.writeMu.Unlock()
	return entry.conn.WriteJSON(msg)
}

// Broadcast sends a JSON message to all connected clients, serialized per connection.
func (m *ConnectionManager) Broadcast(msg interface{}) {
	m.mu.RLock()
	entries := make([]*connEntry, 0, len(m.connections))
	for _, e := range m.connections {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	for _, e := range entries {
		e.writeMu.Lock()
		if err := e.conn.WriteJSON(msg); err != nil {
			m.log.Debug("Broadcast send error", "error", err)
		}
		e.writeMu.Unlock()
	}
}

// ActiveClientIDs returns the set of registered client IDs.
func (m *ConnectionManager) ActiveClientIDs() map[string]struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make(map[string]struct{})
	for _, e := range m.connections {
		if e.clientID != "unknown" && e.clientID != "" {
			ids[e.clientID] = struct{}{}
		}
	}
	return ids
}

// IsClientConnected checks if a client_id still has any active connection.
func (m *ConnectionManager) IsClientConnected(clientID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.connections {
		if e.clientID == clientID {
			return true
		}
	}
	return false
}

func (m *ConnectionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connections)
}
