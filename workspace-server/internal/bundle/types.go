// Package bundle handles workspace bundle export and import.
package bundle

// Bundle is the portable .bundle.json format for a workspace.
type Bundle struct {
	Schema      string            `json:"schema"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Tier        int               `json:"tier"`
	Model       string            `json:"model"`
	SystemPrompt string           `json:"system_prompt"`
	Skills      []BundleSkill     `json:"skills"`
	Tools       []BundleTool      `json:"tools"`
	Prompts     map[string]string `json:"prompts"`
	SubWorkspaces []Bundle        `json:"sub_workspaces"`
	AgentCard   interface{}       `json:"agent_card"`
	Author      string            `json:"author"`
	Version     string            `json:"version"`
}

// BundleSkill is a skill serialized in a bundle.
type BundleSkill struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Files       map[string]string `json:"files"` // relative path → content
}

// BundleTool is a built-in tool reference in a bundle.
type BundleTool struct {
	ID     string                 `json:"id"`
	Config map[string]interface{} `json:"config"`
}
