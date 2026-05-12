package bundle

import (
	"testing"
)

func TestBuildBundleConfigFiles_emptyBundle(t *testing.T) {
	b := &Bundle{}
	files := buildBundleConfigFiles(b)
	if len(files) != 0 {
		t.Errorf("expected empty map for empty bundle, got %d entries", len(files))
	}
}

func TestBuildBundleConfigFiles_systemPrompt(t *testing.T) {
	b := &Bundle{SystemPrompt: "You are a helpful assistant."}
	files := buildBundleConfigFiles(b)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if string(files["system-prompt.md"]) != "You are a helpful assistant." {
		t.Errorf("unexpected system prompt content: %q", files["system-prompt.md"])
	}
}

func TestBuildBundleConfigFiles_configYaml(t *testing.T) {
	b := &Bundle{Prompts: map[string]string{
		"config.yaml": "runtime: langgraph\nmodel: claude-sonnet-4-20250514\n",
	}}
	files := buildBundleConfigFiles(b)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if string(files["config.yaml"]) != "runtime: langgraph\nmodel: claude-sonnet-4-20250514\n" {
		t.Errorf("unexpected config.yaml content: %q", files["config.yaml"])
	}
}

func TestBuildBundleConfigFiles_systemPromptAndConfigYaml(t *testing.T) {
	b := &Bundle{
		SystemPrompt: "# System",
		Prompts:     map[string]string{"config.yaml": "runtime: langgraph"},
	}
	files := buildBundleConfigFiles(b)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if _, ok := files["system-prompt.md"]; !ok {
		t.Error("missing system-prompt.md")
	}
	if _, ok := files["config.yaml"]; !ok {
		t.Error("missing config.yaml")
	}
}

func TestBuildBundleConfigFiles_skills(t *testing.T) {
	b := &Bundle{
		Skills: []BundleSkill{
			{
				ID:          "web-search",
				Name:        "Web Search",
				Description: "Search the web",
				Files:       map[string]string{"readme.md": "# Web Search"},
			},
			{
				ID:          "code-runner",
				Name:        "Code Runner",
				Description: "Execute code",
				Files:       map[string]string{"handler.py": "print('hello')"},
			},
		},
	}
	files := buildBundleConfigFiles(b)
	if len(files) != 2 {
		t.Fatalf("expected 2 skill files, got %d", len(files))
	}

	if content, ok := files["skills/web-search/readme.md"]; !ok {
		t.Error("missing skills/web-search/readme.md")
	} else if string(content) != "# Web Search" {
		t.Errorf("unexpected readme.md: %q", content)
	}

	if _, ok := files["skills/code-runner/handler.py"]; !ok {
		t.Error("missing skills/code-runner/handler.py")
	}
}

func TestBuildBundleConfigFiles_skillsWithSubPaths(t *testing.T) {
	b := &Bundle{
		Skills: []BundleSkill{
			{
				ID:    "nested-skill",
				Files: map[string]string{"src/main.py": "def main(): pass", "pyproject.toml": "[tool.foo]"},
			},
		},
	}
	files := buildBundleConfigFiles(b)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if _, ok := files["skills/nested-skill/src/main.py"]; !ok {
		t.Error("missing skills/nested-skill/src/main.py")
	}
	if _, ok := files["skills/nested-skill/pyproject.toml"]; !ok {
		t.Error("missing skills/nested-skill/pyproject.toml")
	}
}

func TestBuildBundleConfigFiles_skipsEmptyPrompts(t *testing.T) {
	b := &Bundle{Prompts: map[string]string{}}
	files := buildBundleConfigFiles(b)
	if len(files) != 0 {
		t.Errorf("expected 0 files for empty prompts map, got %d", len(files))
	}
}

func TestBuildBundleConfigFiles_skipsMissingConfigYaml(t *testing.T) {
	b := &Bundle{
		SystemPrompt: "# My Prompt",
		Prompts:      map[string]string{"other.yaml": "something: else"},
	}
	files := buildBundleConfigFiles(b)
	if len(files) != 1 {
		t.Fatalf("expected 1 file (system-prompt only), got %d", len(files))
	}
	if _, ok := files["config.yaml"]; ok {
		t.Error("config.yaml should not be written when not in Prompts")
	}
}

func TestNilIfEmpty_emptyString(t *testing.T) {
	result := nilIfEmpty("")
	if result != nil {
		t.Errorf("expected nil for empty string, got %v", result)
	}
}

func TestNilIfEmpty_nonEmptyString(t *testing.T) {
	result := nilIfEmpty("hello")
	if result == nil {
		t.Fatal("expected non-nil result for non-empty string")
	}
	if result != "hello" {
		t.Errorf("expected hello, got %q", result)
	}
}

func TestNilIfEmpty_whitespaceString(t *testing.T) {
	// Whitespace is not empty — nilIfEmpty only checks for zero-length
	result := nilIfEmpty("   ")
	if result == nil {
		t.Error("expected non-nil for whitespace string")
	} else if result != "   " {
		t.Errorf("expected '   ', got %q", result)
	}
}
