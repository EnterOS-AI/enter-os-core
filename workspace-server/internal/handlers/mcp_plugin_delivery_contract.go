package handlers

// MCP-plugin delivery contract (core#3080).
//
// This is the producer-side guard for the MCP-plugin delivery contract
// between molecule-core (which ships plugin declarations) and the
// claude-code workspace template (whose runtime writes/reads
// /configs/.claude/settings.json under the mcpServers key).
//
// The contract file is duplicated byte-for-byte in both repos and kept
// honest by a cross-repo drift gate.

import (
	"encoding/json"
	"os"
)

const mcpPluginDeliveryContractPath = "../../../contracts/mcp-plugin-delivery.contract.json"

// MCPPluginDeliveryContract describes the pinned MCP-plugin delivery surface.
type MCPPluginDeliveryContract struct {
	SettingsPath string `json:"settings_path"`
	Key          string `json:"key"`
	EntryShape   string `json:"entry_shape"`
	Producer     string `json:"producer"`
	Consumer     string `json:"consumer"`
	// MCPServerName is the SSOT for the platform MCP server NAME — the mcpServers
	// key the runtime registers the server under, which Claude Code turns into
	// the tool-id prefix `mcp__<MCPServerName>__<tool>`. The platform-agent
	// template's mcp_servers.yaml and the online/degraded gate's expected tool id
	// (conciergePlatformMCPCreateWorkspaceTool) both derive from this value — see
	// TestSSOT_DegradeGateToolDerivesFromContract.
	MCPServerName string `json:"mcp_server_name"`
}

// LoadMCPPluginDeliveryContract loads the contract from the repo root.
func LoadMCPPluginDeliveryContract() (*MCPPluginDeliveryContract, error) {
	data, err := os.ReadFile(mcpPluginDeliveryContractPath)
	if err != nil {
		return nil, err
	}
	var c MCPPluginDeliveryContract
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
