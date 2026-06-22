package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// rfbGreeting is the first frame a real websockify/RFB backend writes on
// connect. The fake backend below sends these exact bytes so the positive
// test can prove the upstream's first binary frame survives the reverse
// proxy chain (the "WS 1006" regression surface from core#2247 was the
// upgrade/handshake silently failing before any RFB byte reached the
// browser).
var rfbGreeting = []byte("RFB 003.008\n")

// newFakeWebsockifyBackend stands up an httptest.NewServer that upgrades the
// websocket, writes the RFB greeting as a binary frame, then echoes every
// frame it receives back to the client. No EC2, noVNC, or SSH involved — it
// is the stand-in for the on-instance :6080 websockify listener that
// realDisplayForward would normally tunnel to.
func newFakeWebsockifyBackend(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		// The proxy rewrites Sec-WebSocket-Protocol to "binary"; accept any
		// origin/subprotocol so the fake backend never rejects the handshake.
		CheckOrigin:       func(*http.Request) bool { return true },
		Subprotocols:      []string{"binary"},
		HandshakeTimeout:  5 * time.Second,
		EnableCompression: false,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.BinaryMessage, rfbGreeting); err != nil {
			return
		}
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wireDisplayForwardToBackend overrides the injectable displayForward package
// var so DisplaySession proxies to the fake backend instead of opening an EIC
// SSH tunnel. Restored via t.Cleanup. The returned *url.URL is the http://
// backend address (the reverse proxy upgrades it to ws:// natively under
// Go 1.25's ReverseProxy WebSocket support).
func wireDisplayForwardToBackend(t *testing.T, backendURL string) {
	t.Helper()
	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend URL %q: %v", backendURL, err)
	}
	prev := displayForward
	displayForward = func(_ context.Context, _ string, fn func(target *url.URL) error) error {
		return fn(target)
	}
	t.Cleanup(func() { displayForward = prev })
}

// newDisplaySessionTestServer mounts DisplaySession on a gin router behind an
// httptest.NewServer so a real websocket client can dial the route end-to-end.
// It returns the base ws:// URL for the websockify route.
func newDisplaySessionTestServer(t *testing.T, handler *WorkspaceHandler) *httptest.Server {
	t.Helper()
	r := gin.New()
	// Mirror the production registration in internal/router/router.go:
	//   GET /workspaces/:id/display/session/*proxyPath -> wh.DisplaySession
	r.GET("/workspaces/:id/display/session/*proxyPath", handler.DisplaySession)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

const (
	displayProxyWorkspaceID  = "ws-display"
	displayProxyInstanceID   = "i-0fakedeadbeef00001"
	displayProxyControlledBy = "admin-token"
)

// expectDisplaySessionTargetRow mocks loadWorkspaceDisplaySessionTarget's
// workspaces SELECT. mode "desktop-control" + a non-empty instance_id is the
// "display enabled, tunnel available" shape. (Note: the compute validator
// accepts modes none/desktop-control/gpu-desktop-control and protocols
// dcv/novnc — "novnc" is a *protocol*, not a mode, so the enabled rows use
// mode=desktop-control,protocol=novnc.)
func expectDisplaySessionTargetRow(mock sqlmock.Sqlmock, computeJSON, instanceID string) {
	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(displayProxyWorkspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(computeJSON, instanceID))
}

// expectActiveDisplayControlRow mocks loadActiveDisplayControl's locks SELECT
// returning an active lock owned by controlledBy expiring at expiresAt.
func expectActiveDisplayControlRow(mock sqlmock.Sqlmock, controlledBy string, expiresAt time.Time) {
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs(displayProxyWorkspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("user", controlledBy, expiresAt))
}

const enabledComputeJSON = `{"display":{"mode":"desktop-control","protocol":"novnc","width":1280,"height":800}}`

// dialDisplaySession dials the websockify route on the given test server with
// the supplied Sec-WebSocket-Protocol values. It returns the conn (nil on
// failure), the HTTP response, and the dial error.
func dialDisplaySession(t *testing.T, srv *httptest.Server, subprotocols []string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/workspaces/" + displayProxyWorkspaceID + "/display/session/websockify"
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		Subprotocols:     subprotocols,
	}
	return dialer.Dial(wsURL, nil)
}

// TestDisplaySessionProxy_Positive proves the full take-control WS-proxy path
// without any network/EC2: a valid signed token + active lock + enabled
// display upgrades successfully (HTTP 101), the backend's RFB greeting arrives
// through the proxy, and a client->server byte round-trips back (bidirectional
// proxy chain). This is the direct regression guard for the "WS 1006" failure
// class in core#2247.
func TestDisplaySessionProxy_Positive(t *testing.T) {
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "test-secret")
	mock := setupTestDB(t)
	backend := newFakeWebsockifyBackend(t)
	wireDisplayForwardToBackend(t, backend.URL)

	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	srv := newDisplaySessionTestServer(t, handler)

	expiresAt := time.Now().Add(5 * time.Minute)
	expectDisplaySessionTargetRow(mock, enabledComputeJSON, displayProxyInstanceID)
	expectActiveDisplayControlRow(mock, displayProxyControlledBy, expiresAt)

	token := signDisplaySessionToken(displayProxyWorkspaceID, displayProxyControlledBy, expiresAt)
	if token == "" {
		t.Fatal("signDisplaySessionToken returned empty token")
	}

	conn, resp, err := dialDisplaySession(t, srv, []string{"binary", displaySessionTokenProtocolPrefix + token})
	if err != nil {
		body := ""
		if resp != nil {
			body = resp.Status
		}
		t.Fatalf("websocket dial failed: %v (resp=%s)", err, body)
	}
	t.Cleanup(func() { conn.Close() })
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101 Switching Protocols, got %d", resp.StatusCode)
	}

	// 1. The backend's RFB greeting must arrive through the proxy.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read greeting through proxy failed: %v", err)
	}
	if mt != websocket.BinaryMessage || string(msg) != string(rfbGreeting) {
		t.Fatalf("greeting = %q (type %d), want %q binary", msg, mt, rfbGreeting)
	}

	// 2. A client->server byte must echo back (bidirectional chain).
	probe := []byte{0x13, 0x37, 0x00, 0xff}
	if err := conn.WriteMessage(websocket.BinaryMessage, probe); err != nil {
		t.Fatalf("write probe through proxy failed: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, echo, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read echo through proxy failed: %v", err)
	}
	if string(echo) != string(probe) {
		t.Fatalf("echo = %q, want %q", echo, probe)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDisplaySessionProxy_Rejections is table-driven over the failure surface.
// Each case asserts the WS upgrade does NOT happen (dial errors / no 101) and
// the right HTTP status is returned, WITHOUT ever reaching the fake backend.
func TestDisplaySessionProxy_Rejections(t *testing.T) {
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "test-secret")
	pastExpiry := time.Now().Add(-5 * time.Minute)
	futureExpiry := time.Now().Add(5 * time.Minute)

	cases := []struct {
		name string
		// expect wires the sqlmock rows that the handler will actually read
		// for this case (the locks SELECT is only reached for token cases).
		expect func(mock sqlmock.Sqlmock)
		// subprotocols sent on the dial (token header, if any).
		subprotocols []string
		// proxyPath overrides the default "/websockify" route segment.
		proxyPath  string
		wantStatus int
	}{
		{
			name: "missing token -> 403",
			expect: func(m sqlmock.Sqlmock) {
				expectDisplaySessionTargetRow(m, enabledComputeJSON, displayProxyInstanceID)
				expectActiveDisplayControlRow(m, displayProxyControlledBy, futureExpiry)
			},
			subprotocols: []string{"binary"},
			wantStatus:   http.StatusForbidden,
		},
		{
			name: "tampered token -> 403",
			expect: func(m sqlmock.Sqlmock) {
				expectDisplaySessionTargetRow(m, enabledComputeJSON, displayProxyInstanceID)
				expectActiveDisplayControlRow(m, displayProxyControlledBy, futureExpiry)
			},
			subprotocols: []string{"binary", displaySessionTokenProtocolPrefix + "garbage.not-a-valid-mac"},
			wantStatus:   http.StatusForbidden,
		},
		{
			name: "expired lock -> 403",
			expect: func(m sqlmock.Sqlmock) {
				expectDisplaySessionTargetRow(m, enabledComputeJSON, displayProxyInstanceID)
				// Active-lock query filters expires_at > now(), so an
				// expired lock returns no rows -> found=false -> 403.
				m.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
					WithArgs(displayProxyWorkspaceID).
					WillReturnError(sql.ErrNoRows)
			},
			// Token signed against the past expiry would also fail validation
			// even if a stale lock row were returned.
			subprotocols: []string{"binary", displaySessionTokenProtocolPrefix +
				signDisplaySessionToken(displayProxyWorkspaceID, displayProxyControlledBy, pastExpiry)},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "display mode none -> 404",
			expect: func(m sqlmock.Sqlmock) {
				expectDisplaySessionTargetRow(m, `{"display":{"mode":"none"}}`, displayProxyInstanceID)
			},
			subprotocols: []string{"binary"},
			wantStatus:   http.StatusNotFound,
		},
		{
			name: "empty instance_id -> 503",
			expect: func(m sqlmock.Sqlmock) {
				expectDisplaySessionTargetRow(m, enabledComputeJSON, "")
			},
			subprotocols: []string{"binary"},
			wantStatus:   http.StatusServiceUnavailable,
		},
		{
			name: "wrong proxyPath -> 404",
			expect: func(m sqlmock.Sqlmock) {
				expectDisplaySessionTargetRow(m, enabledComputeJSON, displayProxyInstanceID)
			},
			subprotocols: []string{"binary"},
			proxyPath:    "/frames",
			wantStatus:   http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			// A backend that fatals if it is ever reached — proves these
			// rejections happen strictly before any proxy dial.
			reached := false
			backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached = true
			}))
			t.Cleanup(backend.Close)
			wireDisplayForwardToBackend(t, backend.URL)

			handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
			srv := newDisplaySessionTestServer(t, handler)
			tc.expect(mock)

			proxyPath := tc.proxyPath
			if proxyPath == "" {
				proxyPath = "/websockify"
			}
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") +
				"/workspaces/" + displayProxyWorkspaceID + "/display/session" + proxyPath
			dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Subprotocols: tc.subprotocols}
			conn, resp, err := dialer.Dial(wsURL, nil)
			if conn != nil {
				conn.Close()
			}
			if err == nil {
				t.Fatalf("expected WS upgrade to fail, but dial succeeded")
			}
			if resp == nil {
				t.Fatalf("expected an HTTP response on rejected upgrade, got nil (err=%v)", err)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if resp.StatusCode == http.StatusSwitchingProtocols {
				t.Fatalf("upgrade unexpectedly succeeded (101)")
			}
			if reached {
				t.Fatalf("rejection leaked to the upstream backend")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}
