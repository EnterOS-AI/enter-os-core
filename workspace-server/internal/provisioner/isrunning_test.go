package provisioner

import (
	"errors"
	"testing"
)

// isContainerNotFound is the chokepoint that decides whether IsRunning
// tears down a workspace. Getting this wrong (false positive) causes
// the restart cascade observed 2026-04-16 09:10 UTC when 6 containers
// got simultaneous A2A forward failures, their reactive IsRunning
// checks all hit a busy Docker daemon, timed out, and got flipped to
// "dead" in parallel. These tests pin the truth table.

func TestIsContainerNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"docker not-found message",
			errors.New(`Error response from daemon: No such container: ws-abc123`),
			true},
		{"generic not found",
			errors.New("container not found"),
			true},
		{"context deadline",
			errors.New("context deadline exceeded"),
			false},
		{"socket EOF",
			errors.New(`Get "http://%2Fvar%2Frun%2Fdocker.sock/...": EOF`),
			false},
		{"daemon connection refused",
			errors.New("dial unix /var/run/docker.sock: connect: connection refused"),
			false},
		{"i/o timeout",
			errors.New("read unix @->/var/run/docker.sock: i/o timeout"),
			false},
		{"empty string",
			errors.New(""),
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isContainerNotFound(tc.err); got != tc.want {
				t.Errorf("isContainerNotFound(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// isRemovalInProgress decides whether Stop() treats Docker's "already
// being removed" race as success (the container WILL be gone) versus
// surfacing a 500 to the caller. False negative on cascade-delete
// breaks the UX ("workspace marked removed, but stop call(s) failed —
// please retry" when the workspace is, in fact, removed). False
// positive would silently swallow a different daemon error and skip
// the volume cleanup. Both directions matter — pin the truth table.
func TestIsRemovalInProgress(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"docker race message",
			errors.New(`Error response from daemon: removal of container ws-eeb99b5d-607 is already in progress`),
			true},
		{"docker race without ws prefix",
			errors.New(`removal of container abc123 is already in progress`),
			true},
		// "already in progress" alone is too generic — would false-
		// positive on e.g. "image pull is already in progress". Both
		// substrings must be present.
		{"unrelated already in progress",
			errors.New(`image pull is already in progress`),
			false},
		{"not-found is NOT removal-in-progress",
			errors.New(`Error response from daemon: No such container: ws-abc`),
			false},
		{"context deadline",
			errors.New("context deadline exceeded"),
			false},
		{"empty string",
			errors.New(""),
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRemovalInProgress(tc.err); got != tc.want {
				t.Errorf("isRemovalInProgress(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
