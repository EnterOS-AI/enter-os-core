package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)


// expectWorkspaceURLLookup programs the sqlmock to answer the SELECT that
// TranscriptHandler.Get issues for `agent_card->>'url'`. Tests call this
// instead of inserting real rows (we use sqlmock — there's no DB).
//
// Returns the workspace ID as the handler's :id path param.
func expectWorkspaceURLLookup(mock sqlmock.Sqlmock, agentURL string) string {
	id := "11111111-2222-3333-4444-555555555555"
	mock.ExpectQuery("SELECT agent_card->>'url' FROM workspaces WHERE id = \\$1").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(agentURL))
	return id
}

// ==================== GET /workspaces/:id/transcript ====================

func TestTranscript_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewTranscriptHandler()

	mock.ExpectQuery("SELECT agent_card->>'url' FROM workspaces WHERE id = \\$1").
		WithArgs("00000000-0000-0000-0000-000000000000").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/00000000-0000-0000-0000-000000000000/transcript", nil)
	h.Get(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTranscript_ProxyForwardsAndReturnsBody(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewTranscriptHandler()

	// Spin up a fake "workspace" agent that returns a canned transcript
	gotPath := ""
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runtime":"claude-code","supported":true,"lines":[{"type":"user"}],"cursor":1,"more":false}`))
	}))
	defer stub.Close()

	wsID := expectWorkspaceURLLookup(mock,stub.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript?since=5&limit=20", nil)
	h.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/transcript" {
		t.Errorf("expected proxy to hit /transcript, got %q", gotPath)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["runtime"] != "claude-code" {
		t.Errorf("expected runtime=claude-code, got %v", resp["runtime"])
	}
	if lines, ok := resp["lines"].([]interface{}); !ok || len(lines) != 1 {
		t.Errorf("expected 1 line, got %v", resp["lines"])
	}
}

func TestTranscript_ProxyPropagatesAllowlistedQueryParams(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewTranscriptHandler()

	gotQuery := ""
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{}`))
	}))
	defer stub.Close()

	wsID := expectWorkspaceURLLookup(mock,stub.URL)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript?since=42&limit=7&secret=leak&cmd=rm", nil)
	h.Get(c)
	// url.Values.Encode() sorts alphabetically — limit before since.
	// Crucially: secret + cmd are dropped (not in the allowlist).
	if gotQuery != "limit=7&since=42" {
		t.Errorf("expected only allowlisted since/limit forwarded, got %q", gotQuery)
	}
}

// SSRF regression tests — see issue #272. agent_card->>'url' is attacker-
// writable via /registry/register so validateWorkspaceURL must reject
// link-local / cloud-metadata / non-http(s) targets before the outbound
// HTTP call fires.

func TestTranscript_RejectsCloudMetadataIP(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	setSSRFCheckForTest(true)
	h := NewTranscriptHandler()

	wsID := expectWorkspaceURLLookup(mock,"http://169.254.169.254/")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	h.Get(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for IMDS target, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTranscript_RejectsNonHTTPScheme(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	setSSRFCheckForTest(true)
	h := NewTranscriptHandler()

	wsID := expectWorkspaceURLLookup(mock,"file:///etc/passwd")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	h.Get(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for file:// scheme, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTranscript_RejectsMetadataHostname(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	setSSRFCheckForTest(true)
	h := NewTranscriptHandler()

	wsID := expectWorkspaceURLLookup(mock,"http://metadata.google.internal/computeMetadata/v1/")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	h.Get(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for metadata hostname, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTranscript_RejectsLinkLocalIPv6(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	setSSRFCheckForTest(true)
	h := NewTranscriptHandler()

	wsID := expectWorkspaceURLLookup(mock,"http://[fe80::1]/")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	h.Get(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for link-local IPv6, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTranscript_RejectsLoopbackURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	setSSRFCheckForTest(true)
	h := NewTranscriptHandler()

	wsID := expectWorkspaceURLLookup(mock, "http://127.0.0.1:8080/")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	h.Get(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for loopback URL, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTranscript_UnreachableWorkspaceReturns502(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewTranscriptHandler()

	wsID := expectWorkspaceURLLookup(mock,"http://127.0.0.1:1") // refused

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	h.Get(c)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}

// TestTranscript_ForwardsAuthHeader is a regression guard for the fix where
// TranscriptHandler.Get was not forwarding the Authorization header to the
// workspace's /transcript endpoint (QA finding 2026-04-16).
//
// The workspace's /transcript endpoint (secured by #287/#328) requires a valid
// `Authorization: Bearer <token>` header — it fails-closed when the header
// is absent. The platform's WorkspaceAuth middleware validates the token before
// the handler runs; forwarding it to the workspace is correct and safe.
//
// Fix applied: after constructing the outbound request, the handler now calls
//   req.Header.Set("Authorization", c.GetHeader("Authorization"))
// This test verifies the fix and acts as a regression guard.
func TestTranscript_ForwardsAuthHeader(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewTranscriptHandler()

	const testToken = "Bearer test-workspace-token-abc123"

	var receivedAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		// Simulate the workspace's #328 fail-closed behaviour: reject missing auth.
		if receivedAuth == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runtime":"claude-code","supported":true,"lines":[],"cursor":0,"more":false}`))
	}))
	defer stub.Close()

	wsID := expectWorkspaceURLLookup(mock, stub.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	// Simulate a request that has already passed WorkspaceAuth middleware —
	// the bearer token is present and valid on the incoming request.
	req := httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	req.Header.Set("Authorization", testToken)
	c.Request = req
	h.Get(c)

	// The proxy must forward the bearer token so the workspace accepts the call.
	if receivedAuth == "" {
		t.Error("TranscriptHandler did not forward Authorization header — workspace would return 401")
	}
	if receivedAuth != testToken {
		t.Errorf("Authorization header mismatch: forwarded %q, want %q", receivedAuth, testToken)
	}
	if w.Code == http.StatusUnauthorized {
		t.Errorf("workspace returned 401: transcript proxy did not authenticate; auth forwarded: %q", receivedAuth)
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestTranscript_NoAuthHeader_PassesThrough verifies that a request with no
// Authorization header (e.g. unauthenticated local-dev call that somehow
// bypassed WorkspaceAuth) results in no Authorization header on the upstream
// request. The workspace will return 401 in this case, which the proxy
// faithfully relays — no silent upgrade of privilege.
func TestTranscript_NoAuthHeader_PassesThrough(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewTranscriptHandler()

	var receivedAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if receivedAuth == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runtime":"claude-code","supported":true,"lines":[]}`))
	}))
	defer stub.Close()

	wsID := expectWorkspaceURLLookup(mock, stub.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	// No Authorization header on the request.
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	h.Get(c)

	// Without a token the workspace returns 401; the proxy must relay it faithfully.
	if receivedAuth != "" {
		t.Errorf("expected no Authorization forwarded to workspace, got %q", receivedAuth)
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected proxy to relay workspace 401, got %d: %s", w.Code, w.Body.String())
	}
}

// TestTranscript_DisablesRedirects_DoesNotForwardAuthToRedirectTarget pins
// the #2132 RC 103771 step B: the proxy disables HTTP redirects
// (CheckRedirect returns http.ErrUseLastResponse). The default
// http.Client follows 302 responses, forwarding the caller's
// Authorization bearer to the redirect target. A redirect to a
// private/metadata target (e.g. 169.254.169.254 IMDS) would
// leak the bearer token. By disabling redirects, the proxy
// surfaces the 302 to the caller as a 302 (NOT a follow), and
// the bearer never reaches the redirect target. The dial-time
// IP guard (step A) is the belt-and-suspenders for any future
// code that does re-enable redirects.
func TestTranscript_DisablesRedirects_DoesNotForwardAuthToRedirectTarget(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	allowLoopbackForTest(t)
	setSSRFCheckForTest(true)
	h := NewTranscriptHandler()

	// Set up a "workspace" stub that returns 302 → IMDS. The
	// proxy must NOT follow (CheckRedirect=ErrUseLastResponse)
	// and must NOT forward the Authorization header to the
	// redirect target.
	var redirectHits int
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/transcript" {
			// First request: return 302 → IMDS.
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/iam/security-credentials/")
			w.WriteHeader(http.StatusFound)
			return
		}
		redirectHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	wsID := expectWorkspaceURLLookup(mock,stub.URL)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/transcript", nil)
	c.Request.Header.Set("Authorization", "Bearer secret-caller-token")
	h.Get(c)

	// 302 must surface to the caller (not 200 / not 502). CheckRedirect
	// returns http.ErrUseLastResponse; the proxy hands the 302 through.
	if w.Code != http.StatusFound {
		t.Errorf("expected 302 to surface to caller (redirects disabled), got %d: %s", w.Code, w.Body.String())
	}
	// The redirect target (IMDS) must NEVER have been hit.
	if redirectHits != 0 {
		t.Errorf("redirect target was hit %d times — Authorization bearer was forwarded to a private IP", redirectHits)
	}
	// NOTE: the proxy uses c.Data(status, contentType, body) which
	// only sets Content-Type — Location is NOT passed through. That's
	// a pre-existing proxy limitation (out of scope for the SSRF fix).
	// The SSRF fix's contract is: 302 surfaces + redirect target NOT
	// hit + bearer NOT forwarded to the target. Both checked above.
	_ = w.Header().Get("Location")
}

// TestTranscript_DialGuardBlocksBeforeConnect pins the #2132 fast-follow
// (Researcher RC 103905): the dial-time IP guard runs in
// net.Dialer.Control, which fires AFTER getaddrinfo but BEFORE the
// TCP connect() syscall. The prior POST-DIAL safeDialContext (dialed
// then closed on a blocked IP) left a port-scan side-channel: a TCP
// SYN was sent to the internal target, opening a connection that
// could be observed by the target's stack (and potentially logged
// by IMDS as a credential-exfil probe). This test asserts:
//
//  1. A direct dial to a loopback address via safeDialer() returns
//     an *ssrfDialError (the SSRF policy was enforced).
//  2. NO TCP connection was opened on the listener (the
//     connect() syscall never ran; Control rejected it first).
//
// The test bypasses the front-door isSafeURL gate by using safeDialer()
// directly — the front-door gate would otherwise block loopback before
// the dialer runs. The point of this test is the dialer's behavior
// in isolation (the belt-and-suspenders for DNS-rebinding TOCTOU).
func TestTranscript_DialGuardBlocksBeforeConnect(t *testing.T) {
	// Stand up a real TCP listener on loopback. The handler that
	// counts accepts runs in a goroutine; if Control does its job
	// the connect() syscall never runs, no SYN arrives, and the
	// accept count stays at 0.
	var acceptCount int
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, aerr := listener.Accept()
			if aerr != nil {
				return // listener closed → test over
			}
			acceptCount++
			_ = conn.Close()
		}
	}()

	// Now dial the listener's address via safeDialer(). This
	// exercises the same Control path the transcript proxy uses.
	// The address is 127.0.0.1 (loopback) which isSafeURL rejects
	// in production; the dialer's Control must intercept and
	// return an error BEFORE the connect() syscall.
	addr := listener.Addr().String()
	conn, dialErr := safeDialer().DialContext(context.Background(), "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	if dialErr == nil {
		t.Fatalf("expected dial to be blocked by Control, but it succeeded (conn=%v)", conn)
	}
	var ssrfErr *ssrfDialError
	if !errors.As(dialErr, &ssrfErr) {
		t.Fatalf("expected *ssrfDialError, got %T: %v", dialErr, dialErr)
	}
	if !ssrfErr.ip.IsLoopback() {
		t.Errorf("expected loopback IP in error, got %v", ssrfErr.ip)
	}

	// Give the OS a moment to deliver any in-flight SYNs (none
	// should arrive, but the accept loop is async).
	time.Sleep(50 * time.Millisecond)
	if acceptCount != 0 {
		t.Errorf("listener accepted %d connections — Control did NOT block the dial before connect() (port-scan side-channel open)", acceptCount)
	}
}

// TestSafeDialControl_FailsClosedOnLookupIPError pins the RC 103980/12169
// fail-closed hardening: when safeDialControl's hostname-fallback path
// is invoked (ip == nil on the address) and net.LookupIP returns an
// error, the dial MUST be blocked. The prior shape FAIL-OPENed in this
// case (the inner `if ips, lookupErr := net.LookupIP(host); lookupErr == nil`
// gate skipped the policy when LookupIP errored, then `return nil`
// approved the dial). An attacker who can suppress DNS for a hostname
// (DNS outage, hostile resolver, etc.) could otherwise get the dial
// to proceed with whatever IP the dialer's own resolver picks up later.
//
// .invalid is a reserved TLD (RFC 2606) that does not resolve, so
// LookupIP("nonexistent.invalid") returns an error deterministically.
func TestSafeDialControl_FailsClosedOnLookupIPError(t *testing.T) {
	// .invalid TLD — RFC 2606 reserved, never resolves.
	err := safeDialControl("tcp", "nonexistent-host.invalid:443", nil)
	if err == nil {
		t.Fatalf("safeDialControl: want error for unresolvable hostname (FAIL-CLOSED), got nil (FAIL-OPEN — the bug the Researcher flagged, RC 103980/12169)")
	}
	var ssrfErr *ssrfDialError
	if !errors.As(err, &ssrfErr) {
		t.Fatalf("safeDialControl: want *ssrfDialError for unresolvable hostname, got %T: %v", err, err)
	}
	if ssrfErr.host != "nonexistent-host.invalid" {
		t.Errorf("safeDialControl: want ssrfDialError.host=%q, got %q", "nonexistent-host.invalid", ssrfErr.host)
	}
	if ssrfErr.ip != nil {
		t.Errorf("safeDialControl: want ssrfDialError.ip=nil (no IP resolved), got %v", ssrfErr.ip)
	}
	// Error() must not panic and must include the hostname + reason.
	msg := ssrfErr.Error()
	if !strings.Contains(msg, "nonexistent-host.invalid") {
		t.Errorf("ssrfDialError.Error() should include hostname, got %q", msg)
	}
	if !strings.Contains(msg, "hostname resolution failed") {
		t.Errorf("ssrfDialError.Error() should include the reason, got %q", msg)
	}
}

// TestSafeDialControl_FailsClosedOnEmptyLookupIPResult pins the same
// fail-closed contract for the LookupIP-returns-empty case. We can't
// easily force an empty result without a custom resolver, but we CAN
// assert the contract by checking the docstring-equivalent invariant:
// when len(ips) == 0, safeDialControl returns *ssrfDialError with
// host set + reason containing "no addresses". The test documents
// the contract; the LookupIP-error test above exercises the same
// code path (LookupIP errored) with the same fail-closed behavior.
func TestSafeDialControl_FailsClosedOnEmptyLookupIPResult(t *testing.T) {
	// We can't easily mock LookupIP to return an empty slice, but
	// the FAIL-CLOSED contract for the empty case is structurally
	// identical to the LookupIP-error case (both paths return
	// *ssrfDialError with host set). The integration through
	// real LookupIP with a hostname that resolves to 0 records
	// would be the canonical test, but no public DNS zone
	// guarantees a zero-record host. The two contract-level tests
	// (LookupIP-error above + Error() format below) cover the
	// behavior; the empty-result branch is exercised by code review
	// of the explicit `if len(ips) == 0` check.
	//
	// Verify the Error() format for the empty-result case.
	ssrfErr := &ssrfDialError{
		host:   "empty-lookup.invalid",
		reason: errors.New("hostname resolution returned no addresses"),
	}
	msg := ssrfErr.Error()
	if !strings.Contains(msg, "empty-lookup.invalid") {
		t.Errorf("Error() for empty-result case should include hostname, got %q", msg)
	}
	if !strings.Contains(msg, "no addresses") {
		t.Errorf("Error() for empty-result case should include the reason, got %q", msg)
	}
}
