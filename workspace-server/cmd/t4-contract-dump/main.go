// Command t4-contract-dump prints the T4 privilege contract as YAML.
//
// Usage:
//
//	go run ./workspace-server/cmd/t4-contract-dump > t4_capabilities.yaml
//
// This is the seam that template-repo CI workflows consume:
//
//	- Template CI fetches molecule-core at pinned ref
//	- Runs `go run ./workspace-server/cmd/t4-contract-dump` to produce
//	  t4_capabilities.yaml
//	- Iterates capabilities and runs each Probe inside a freshly-built
//	  privileged container
//	- Aggregates structured pass/fail; fails the gate on any hard miss.
//
// Keeping this trivial and pure-stdlib means a fork user does not need
// a Molecule-AI Gitea token or any internal infrastructure to consume
// the contract — `go run` against molecule-core's public source is
// enough.
package main

import (
	"fmt"
	"os"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
)

func main() {
	caps := provisioner.T4PrivilegeContract()
	if _, err := os.Stdout.WriteString(provisioner.AsYAML(caps)); err != nil {
		fmt.Fprintln(os.Stderr, "t4-contract-dump: write failed:", err)
		os.Exit(1)
	}
}
