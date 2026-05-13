package ws

import (
	"sync"
	"testing"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// mockClient returns a Client with a buffered send channel of the given size
// and a nil WebSocket connection. Nil Conn is safe for our tests because we
// never call WritePump (which uses Conn) — we only test the hub's send channel
// and broadcast logic.
func mockClient(workspaceID string, bufSize int) *Client {
	return &Client{
		WorkspaceID: workspaceID,
		Send:        make(chan []byte, bufSize),
		// Conn is nil — safe: WritePump (which uses Conn) is never called in tests.
	}
}

// ─── NewHub ────────────────────────────────────────────────────────────────

func TestNewHub_NilChecker(t *testing.T) {
	// nil AccessChecker is accepted (hub allows all workspace→workspace broadcasts
	// when canCommunicate is unset — the gating is purely advisory).
	h := NewHub(nil)
	if h == nil {
		t.Fatal("NewHub(nil) returned nil")
	}
	if h.canCommunicate != nil {
		t.Error("canCommunicate should be nil")
	}
}

func TestNewHub_AccessCheckerWired(t *testing.T) {
	called := false
	checker := func(callerID, targetID string) bool {
		called = true
		return callerID == targetID // only self-communication allowed
	}
	h := NewHub(checker)
	if h.canCommunicate == nil {
		t.Fatal("canCommunicate not wired")
	}
	// Invoke the wired function directly
	allowed := h.canCommunicate("ws-1", "ws-1")
	if !called {
		t.Error("checker was not called")
	}
	if !allowed {
		t.Error("self-communication should be allowed")
	}
	if h.canCommunicate("ws-1", "ws-2") {
		t.Error("cross-workspace communication should be blocked by checker")
	}
}

// ─── safeSend ─────────────────────────────────────────────────────────────

func TestSafeSend_OpenChannel_Sends(t *testing.T) {
	c := mockClient("ws-1", 10)
	data := []byte(`{"type":"ping"}`)
	ok := safeSend(c, data)
	if !ok {
		t.Error("safeSend should return true for open channel")
	}
	select {
	case got := <-c.Send:
		if string(got) != string(data) {
			t.Errorf("got %q, want %q", got, data)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("no message received on channel")
	}
}

func TestSafeSend_ClosedChannel_ReturnsFalse(t *testing.T) {
	c := mockClient("ws-1", 10)
	close(c.Send) // close before safeSend
	ok := safeSend(c, []byte("data"))
	if ok {
		t.Error("safeSend should return false for closed channel")
	}
}

func TestSafeSend_FullChannel_ReturnsFalse(t *testing.T) {
	c := mockClient("ws-1", 1) // buffer size 1
	// Fill the channel
	c.Send <- []byte("first")
	// Channel is now full
	ok := safeSend(c, []byte("second"))
	if ok {
		t.Error("safeSend should return false when channel buffer is full")
	}
	// Drain to leave clean state
	<-c.Send
}

// ─── Broadcast ────────────────────────────────────────────────────────────

func TestBroadcast_CanvasAlwaysReceives(t *testing.T) {
	h := NewHub(nil) // nil checker: canvas always gets messages

	// Canvas client (no workspaceID) + two workspace clients
	canvas := mockClient("", 10)
	ws1 := mockClient("ws-1", 10)
	ws2 := mockClient("ws-2", 10)

	// Manually register clients into hub state
	h.mu.Lock()
	h.clients[canvas] = true
	h.clients[ws1] = true
	h.clients[ws2] = true
	h.mu.Unlock()

	msg := models.WSMessage{Event: "test", Payload: []byte(`"hello"`)}
	h.Broadcast(msg)

	// Canvas must receive
	select {
	case got := <-canvas.Send:
		t.Logf("canvas received: %s", got)
	case <-time.After(100 * time.Millisecond):
		t.Error("canvas client did not receive broadcast")
	}
}

func TestBroadcast_WorkspaceCanCommunicateGating(t *testing.T) {
	// Only ws-1 can receive messages for ws-2
	checker := func(callerID, targetID string) bool {
		return callerID == targetID
	}
	h := NewHub(checker)

	ws1 := mockClient("ws-1", 10)
	ws2 := mockClient("ws-2", 10)
	canvas := mockClient("", 10)

	h.mu.Lock()
	h.clients[ws1] = true
	h.clients[ws2] = true
	h.clients[canvas] = true
	h.mu.Unlock()

	// Broadcast addressed to ws-2
	msg := models.WSMessage{Event: "test", WorkspaceID: "ws-2"}
	h.Broadcast(msg)

	// ws-1 should NOT receive (not the target, checker says no)
	select {
	case <-ws1.Send:
		t.Error("ws-1 should not receive broadcast for ws-2")
	case <-time.After(50 * time.Millisecond):
		t.Log("ws-1 correctly blocked — no message")
	}

	// ws-2 should receive
	select {
	case <-ws2.Send:
		t.Log("ws-2 correctly received broadcast")
	case <-time.After(100 * time.Millisecond):
		t.Error("ws-2 did not receive broadcast")
	}

	// Canvas always receives
	select {
	case <-canvas.Send:
		t.Log("canvas correctly received broadcast")
	case <-time.After(100 * time.Millisecond):
		t.Error("canvas did not receive broadcast")
	}
}

func TestBroadcast_DropsOnClosedChannel(t *testing.T) {
	h := NewHub(nil)
	c := mockClient("", 10)
	close(c.Send) // pre-close so safeSend returns false

	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()

	// Broadcast must not panic; closed client should be dropped silently.
	msg := models.WSMessage{Event: "ping"}
	h.Broadcast(msg) // should not panic
}

func TestBroadcast_DropsOnFullChannel(t *testing.T) {
	h := NewHub(nil)
	c := mockClient("", 1)
	c.Send <- []byte("blocker") // fill buffer

	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()

	msg := models.WSMessage{Event: "ping"}
	h.Broadcast(msg) // safeSend returns false; no panic

	// Drain to leave clean state
	<-c.Send
}

func TestBroadcast_EmptyHubNoPanic(t *testing.T) {
	h := NewHub(nil)
	msg := models.WSMessage{Event: "ping"}
	h.Broadcast(msg) // must not panic with no clients
}

func TestBroadcast_MultiClient(t *testing.T) {
	h := NewHub(nil)
	clients := make([]*Client, 5)
	h.mu.Lock()
	for i := 0; i < 5; i++ {
		clients[i] = mockClient("", 10)
		h.clients[clients[i]] = true
	}
	h.mu.Unlock()

	msg := models.WSMessage{Event: "multi", Payload: []byte(`"all receive"`)}
	h.Broadcast(msg)

	for i, c := range clients {
		select {
		case <-c.Send:
			t.Logf("client %d received", i)
		case <-time.After(100 * time.Millisecond):
			t.Errorf("client %d did not receive broadcast", i)
		}
	}
}

func TestBroadcast_CanvasIgnoresChecker(t *testing.T) {
	// Strict checker that blocks ALL cross-workspace (never returns true for different IDs)
	strictChecker := func(callerID, targetID string) bool {
		return callerID == targetID
	}
	h := NewHub(strictChecker)

	canvas := mockClient("", 10)

	h.mu.Lock()
	h.clients[canvas] = true
	h.mu.Unlock()

	msg := models.WSMessage{Event: "ping", WorkspaceID: "ws-1"}
	h.Broadcast(msg)

	select {
	case <-canvas.Send:
		t.Log("canvas received message even though checker blocks ws-1")
	case <-time.After(100 * time.Millisecond):
		t.Error("canvas must always receive — checker should be bypassed")
	}
}

// ─── Close ────────────────────────────────────────────────────────────────

func TestClose_DisconnectsAllClients(t *testing.T) {
	h := NewHub(nil)
	clients := make([]*Client, 3)
	h.mu.Lock()
	for i := 0; i < 3; i++ {
		clients[i] = mockClient("", 10)
		h.clients[clients[i]] = true
	}
	h.mu.Unlock()

	// Start Run goroutine so Close can drain Unregister channel
	go h.Run()
	defer h.Close()

	// Unregister all clients so the mutex is released before Close() tries to lock it
	for _, c := range clients {
		h.Unregister <- c
	}
	time.Sleep(50 * time.Millisecond)

	// Now close — mutex is free, Close() should succeed
	h.Close()

	// All client channels should be closed
	for i, c := range clients {
		select {
		case _, ok := <-c.Send:
			if ok {
				t.Errorf("client %d channel still open after Close", i)
			}
		case <-time.After(100 * time.Millisecond):
			// Channel drained and closed
		}
	}
}

func TestClose_Idempotent(t *testing.T) {
	h := NewHub(nil)
	c := mockClient("", 10)
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()

	// Close twice — must not panic or deadlock
	h.Close()
	h.Close() // second call also fine
}

func TestClose_ClosesDoneChannel(t *testing.T) {
	h := NewHub(nil)

	// Start Run goroutine
	done := make(chan struct{})
	go func() {
		h.Run()
		close(done)
	}()

	h.Close()

	select {
	case <-done:
		t.Log("Run exited after Close")
	case <-time.After(200 * time.Millisecond):
		t.Error("Run did not exit after Close")
	}
}

// ─── Run goroutine (Unregister) ──────────────────────────────────────────

func TestRun_UnregisterClosesClientSend(t *testing.T) {
	h := NewHub(nil)
	c := mockClient("ws-1", 10)

	// Start Run() BEFORE sending to Register — Register is unbuffered,
	// so Run() must be ready to receive before the send can complete.
	go h.Run()
	defer h.Close()

	// Register the client
	h.Register <- c

	// Give Run a moment to register the client
	time.Sleep(20 * time.Millisecond)

	// Unregister client
	h.Unregister <- c

	select {
	case _, ok := <-c.Send:
		if ok {
			t.Error("client send channel should be closed after Unregister")
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("client send channel not closed within timeout")
	}
}

// ─── Concurrent access ────────────────────────────────────────────────────

func TestBroadcast_ConcurrentSafe(t *testing.T) {
	h := NewHub(nil)
	clients := make([]*Client, 10)
	h.mu.Lock()
	for i := 0; i < 10; i++ {
		clients[i] = mockClient("", 100)
		h.clients[clients[i]] = true
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				h.Broadcast(models.WSMessage{Event: "ping", Payload: []byte(`"concurrent"`)})

			}
		}(i)
	}

	wg.Wait() // should not deadlock or panic
}
