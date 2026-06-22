// e2e-names prints Docker container/volume names for a workspace ID.
//
// This is the shell-side single source of truth (SSOT) for SEV-2499:
// it imports the same Go naming helpers the provisioner uses, so E2E
// scripts can never drift from the real naming convention again.
//
// Usage:
//
//	e2e-names container <workspace-id>
//	e2e-names config-volume <workspace-id>
//	e2e-names session-volume <workspace-id>
//	e2e-names workspace-volume <workspace-id>
package main

import (
	"fmt"
	"os"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: e2e-names <kind> <workspace-id>")
		fmt.Fprintln(os.Stderr, "  kinds: container, config-volume, session-volume, workspace-volume")
		os.Exit(1)
	}

	kind := os.Args[1]
	wsid := os.Args[2]

	switch kind {
	case "container":
		fmt.Println(provisioner.ContainerName(wsid))
	case "config-volume":
		fmt.Println(provisioner.ConfigVolumeName(wsid))
	case "session-volume":
		fmt.Println(provisioner.ClaudeSessionVolumeName(wsid))
	case "workspace-volume":
		fmt.Println(provisioner.WorkspaceVolumeName(wsid))
	default:
		fmt.Fprintf(os.Stderr, "e2e-names: unknown kind %q\n", kind)
		os.Exit(1)
	}
}
