package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestHandleDiagnose_RoutesToRemote pins the dispatch: a workspace row with
// a non-empty instance_id takes the EIC + ssh probe path. We stub the
// first-stage (send-ssh-public-key) to fail so the test stays
// hermetic — no AWS calls, no network — and confirm:
//
//   - first_failure is "send-ssh-public-key" (not the earlier ssh-keygen)
//   - the steps array includes the ssh-keygen pass + the failed
//     send-ssh-public-key step
//   - response is HTTP 200 (the endpoint always returns 200; failure is
//     in the JSON body so callers don't need branch-on-status)
func TestHandleDiagnose_RoutesToRemote(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available in PATH:", err)
	}
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-remote").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow("i-abc123"))

	prev := sendSSHPublicKey
	sendSSHPublicKey = func(ctx context.Context, region, instanceID, osUser, pubKey string) error {
		return errors.New("AccessDeniedException: not authorized")
	}
	defer func() { sendSSHPublicKey = prev }()

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-remote"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-remote/terminal/diagnose", nil)

	h.HandleDiagnose(c)

	if w.Code != 200 {
		t.Fatalf("HandleDiagnose status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var got diagnoseResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v (body=%s)", err, w.Body.String())
	}
	if !got.Remote {
		t.Errorf("Remote=false; expected true for instance_id-bearing workspace")
	}
	if got.OK {
		t.Errorf("OK=true despite stubbed send-key failure")
	}
	if got.FirstFailure != "send-ssh-public-key" {
		t.Errorf("FirstFailure=%q; want send-ssh-public-key", got.FirstFailure)
	}
	// ssh-keygen must run successfully before send-ssh-public-key fails.
	if len(got.Steps) < 2 {
		t.Fatalf("expected >=2 steps (ssh-keygen + send-ssh-public-key); got %d", len(got.Steps))
	}
	if got.Steps[0].Name != "ssh-keygen" || !got.Steps[0].OK {
		t.Errorf("step[0]: want ssh-keygen ok=true; got %+v", got.Steps[0])
	}
	if got.Steps[1].Name != "send-ssh-public-key" || got.Steps[1].OK {
		t.Errorf("step[1]: want send-ssh-public-key ok=false; got %+v", got.Steps[1])
	}
	// The IAM error message must surface in the step's Error field — that's
	// the whole point of the endpoint.
	if got.Steps[1].Error == "" {
		t.Errorf("step[1].Error is empty; AWS error must surface verbatim")
	}
}

// TestHandleDiagnose_RoutesToLocal — empty instance_id takes the Docker
// path. With nil docker client, container-found can't even start, so we
// fail at "docker-available". Confirms the local-vs-remote dispatch.
func TestHandleDiagnose_RoutesToLocal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-local").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-local"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-local/terminal/diagnose", nil)

	h.HandleDiagnose(c)

	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var got diagnoseResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if got.Remote {
		t.Errorf("Remote=true; expected false for empty-instance_id workspace")
	}
	if got.FirstFailure != "docker-available" {
		t.Errorf("FirstFailure=%q; want docker-available (no docker client)", got.FirstFailure)
	}
}

// TestHandleDiagnose_KI005_RejectsCrossWorkspace — the diagnostic endpoint
// has the same cross-workspace info-leak surface as /terminal had before
// #1609. Without KI-005, an org-level token holder could probe any
// workspace in their tenant by guessing the UUID, learning which IAM call
// fails or which sshd error fires. This test pins that HandleDiagnose
// applies the same hierarchy guard as HandleConnect (parity: ws-attacker
// claiming X-Workspace-ID against /workspaces/ws-victim/terminal/diagnose
// must 403, never reaching the SELECT COALESCE for instance_id).
func TestHandleDiagnose_KI005_RejectsCrossWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Stub CanCommunicate to deny. Reset after — same pattern as the
	// HandleConnect KI-005 tests.
	prev := canCommunicateCheck
	canCommunicateCheck = func(callerID, targetID string) bool { return false }
	defer func() { canCommunicateCheck = prev }()

	// Token validation: caller's bearer is bound to ws-attacker.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id\s+FROM workspace_auth_tokens t`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-attacker"))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-victim"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-victim/terminal/diagnose", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-attacker")
	c.Request.Header.Set("Authorization", "Bearer attacker-token")

	h.HandleDiagnose(c)

	if w.Code != 403 {
		t.Errorf("cross-workspace diagnose: got %d, want 403 (%s)", w.Code, w.Body.String())
	}
	// Critically: the SELECT COALESCE for instance_id must NOT have run —
	// no expectation was set for it. ExpectationsWereMet ensures we
	// rejected before reaching the DB lookup.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (rejection should fire before instance_id lookup): %v", err)
	}
}

// TestDiagnoseRemote_StopsAtSSHProbe — full happy path through send-key,
// pick-port, open-tunnel, wait-for-port, then stub the ssh probe to fail.
// Confirms first_failure surfaces the actual ssh stderr ("Permission
// denied") rather than the earlier successful steps. This is the
// most operationally important behavior — the endpoint exists primarily
// to differentiate "IAM broke" (send-key fails) from "sshd broke" (probe
// fails) from "SG/network broke" (wait-for-port fails).
func TestDiagnoseRemote_StopsAtSSHProbe(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available in PATH:", err)
	}
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc not available in PATH:", err)
	}
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-probe-fail").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow("i-test"))

	// Stub send-key to succeed.
	prevSend := sendSSHPublicKey
	sendSSHPublicKey = func(ctx context.Context, region, instanceID, osUser, pubKey string) error {
		return nil
	}
	defer func() { sendSSHPublicKey = prevSend }()

	// Stub openTunnelCmd to spawn `nc -l <port>` so waitForPort succeeds.
	// We need the tunnel to actually bind the port; nc does that
	// portably. macOS has BSD nc by default.
	prevTun := openTunnelCmd
	openTunnelCmd = func(o eicSSHOptions) *exec.Cmd {
		// `nc -l <port>` listens on the picked free port. -k keeps it
		// alive across single-client disconnects on Linux nc; harmless
		// on BSD nc which doesn't have it (we'd need -k for BSD too —
		// fall back to a portable busy-wait).
		return exec.Command("sh", "-c",
			`port="$1"; while true; do nc -l "$port" >/dev/null 2>&1 || true; done`,
			"sh", strconv.Itoa(o.LocalPort))
	}
	defer func() { openTunnelCmd = prevTun }()

	// Stub the ssh probe to return "Permission denied" with non-zero exit,
	// the canonical "key wasn't authorized" failure.
	prevProbe := sshProbeCmd
	sshProbeCmd = func(o eicSSHOptions) *exec.Cmd {
		return exec.Command("sh", "-c", "echo 'Permission denied (publickey).' >&2; exit 255")
	}
	defer func() { sshProbeCmd = prevProbe }()

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-probe-fail"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-probe-fail/terminal/diagnose", nil)

	h.HandleDiagnose(c)

	if w.Code != 200 {
		t.Fatalf("status: got %d", w.Code)
	}
	var got diagnoseResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v (body=%s)", err, w.Body.String())
	}
	if got.OK {
		t.Errorf("OK=true despite stubbed probe failure")
	}
	if got.FirstFailure != "ssh-probe" {
		t.Errorf("FirstFailure=%q; want ssh-probe (got body=%s)", got.FirstFailure, w.Body.String())
	}
	// The "Permission denied" message must be in the probe step's Detail —
	// that's what tells the operator "this is sshd auth, not network".
	var probeStep *diagnoseStep
	for i := range got.Steps {
		if got.Steps[i].Name == "ssh-probe" {
			probeStep = &got.Steps[i]
			break
		}
	}
	if probeStep == nil {
		t.Fatalf("no ssh-probe step in result: %+v", got.Steps)
	}
	if probeStep.OK {
		t.Errorf("ssh-probe step OK=true despite failure stub")
	}
	if probeStep.Detail == "" && probeStep.Error == "" {
		t.Errorf("ssh-probe step has no Error or Detail; ssh stderr is exactly what we want to expose")
	}
}

// TestUnwrapGoError pins the unwrapGoError helper that extracts subprocess
// stderr from the Go-wrapped error string produced by sendSSHPublicKey.
// Regression gate for mc#687: the E2E smoke now reads detail (not error),
// so detail MUST contain the actionable AWS permission signal.
func TestUnwrapGoError(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
	}{
		{
			name:  "AWS permission denied",
			input: "send-ssh-public-key: exec: exit status 1 (AccessDeniedException: User: arn:aws:iam::123456789012:role/TestRole is not authorized to perform: ec2-instance-connect:OpenTunnel)",
			want:  "AccessDeniedException: User: arn:aws:iam::123456789012:role/TestRole is not authorized to perform: ec2-instance-connect:OpenTunnel",
		},
		{
			name:  "generic exec error no output",
			input: "send-ssh-public-key: exec: exit status 1",
			want:  "",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "short string below threshold",
			input: "err",
			want:  "",
		},
		{
			name:  "no parentheses",
			input: "send-ssh-public-key: something went wrong",
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unwrapGoError(tc.input)
			if got != tc.want {
				t.Errorf("unwrapGoError(%q): got %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

