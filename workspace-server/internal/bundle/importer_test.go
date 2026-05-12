package bundle

import (
	"testing"
)

func TestBuildBundleConfigFiles_EmptyBundle(t *testing.T) {
	b := &Bundle{}
	files := buildBundleConfigFiles(b)
	if len(files) != 0 {
		t.Errorf("empty bundle: want 0 files, got %d", len(files))
	}
}

func TestBuildBundleConfigFiles_SystemPromptOnly(t *testing.T) {
	b := &Bundle{
		SystemPrompt: "You are a helpful assistant.",
	}
	files := buildBundleConfigFiles(b)
	if n := len(files); n != 1 {
		t.Fatalf("system-prompt only: want 1 file, got %d", n)
	}
	if content, ok := files["system-prompt.md"]; !ok {
		t.Fatal("missing system-prompt.md")
	} else if string(content) != "You are a helpful assistant." {
		t.Errorf("system-prompt content: got %q", string(content))
	}
}

func TestBuildBundleConfigFiles_ConfigYamlOnly(t *testing.T) {
	b := &Bundle{
		Prompts: map[string]string{
			"config.yaml": "runtime: langgraph\ntier: 2\n",
		},
	}
	files := buildBundleConfigFiles(b)
	if n := len(files); n != 1 {
		t.Fatalf("config.yaml only: want 1 file, got %d", n)
	}
	if content, ok := files["config.yaml"]; !ok {
		t.Fatal("missing config.yaml")
	} else if string(content) != "runtime: langgraph\ntier: 2\n" {
		t.Errorf("config.yaml content: got %q", string(content))
	}
}

func TestBuildBundleConfigFiles_SystemPromptAndConfigYaml(t *testing.T) {
	b := &Bundle{
		SystemPrompt: "Be concise.",
		Prompts: map[string]string{
			"config.yaml": "runtime: langgraph\n",
		},
	}
	files := buildBundleConfigFiles(b)
	if n := len(files); n != 2 {
		t.Fatalf("system-prompt + config.yaml: want 2 files, got %d", n)
	}
	if _, ok := files["system-prompt.md"]; !ok {
		t.Error("missing system-prompt.md")
	}
	if _, ok := files["config.yaml"]; !ok {
		t.Error("missing config.yaml")
	}
}

func TestBuildBundleConfigFiles_Skills(t *testing.T) {
	b := &Bundle{
		Skills: []BundleSkill{
			{
				ID:   "web-search",
				Files: map[string]string{"readme.md": "# Web Search\n"},
			},
			{
				ID:   "code-interpreter",
				Files: map[string]string{"readme.md": "# Code Interpreter\n"},
			},
		},
	}
	files := buildBundleConfigFiles(b)
	// 2 skills × 1 file each = 2 files
	if n := len(files); n != 2 {
		t.Fatalf("skills: want 2 files, got %d", n)
	}
	if _, ok := files["skills/web-search/readme.md"]; !ok {
		t.Error("missing skills/web-search/readme.md")
	}
	if _, ok := files["skills/code-interpreter/readme.md"]; !ok {
		t.Error("missing skills/code-interpreter/readme.md")
	}
}

func TestBuildBundleConfigFiles_SkillSubPaths(t *testing.T) {
	b := &Bundle{
		Skills: []BundleSkill{
			{
				ID: "multi-file",
				Files: map[string]string{
					"readme.md":   "# Multi",
					"instructions.txt": "Step 1, Step 2",
				},
			},
		},
	}
	files := buildBundleConfigFiles(b)
	if n := len(files); n != 2 {
		t.Fatalf("skill with sub-paths: want 2 files, got %d", n)
	}
	if _, ok := files["skills/multi-file/readme.md"]; !ok {
		t.Error("missing skills/multi-file/readme.md")
	}
	if _, ok := files["skills/multi-file/instructions.txt"]; !ok {
		t.Error("missing skills/multi-file/instructions.txt")
	}
}

func TestBuildBundleConfigFiles_EmptySystemPrompt(t *testing.T) {
	b := &Bundle{
		SystemPrompt: "",
		Prompts: map[string]string{
			"config.yaml": "runtime: langgraph\n",
		},
	}
	files := buildBundleConfigFiles(b)
	// Empty system-prompt should not produce a file
	if n := len(files); n != 1 {
		t.Errorf("empty system-prompt: want 1 file, got %d", n)
	}
}

func TestBuildBundleConfigFiles_EmptyPrompts(t *testing.T) {
	b := &Bundle{
		Prompts: map[string]string{},
	}
	files := buildBundleConfigFiles(b)
	if n := len(files); n != 0 {
		t.Errorf("empty prompts map: want 0 files, got %d", n)
	}
}

// nilIfEmpty

func TestNilIfEmpty_EmptyString(t *testing.T) {
	got := nilIfEmpty("")
	if got != nil {
		t.Errorf("nilIfEmpty(\"\"): want nil, got %v", got)
	}
}

func TestNilIfEmpty_NonEmptyString(t *testing.T) {
	got := nilIfEmpty("hello")
	if got == nil {
		t.Fatal("nilIfEmpty(\"hello\"): want \"hello\", got nil")
	}
	if s, ok := got.(string); !ok || s != "hello" {
		t.Errorf("nilIfEmpty(\"hello\"): got %v (%T)", got, got)
	}
}

func TestNilIfEmpty_Whitespace(t *testing.T) {
	got := nilIfEmpty("   ")
	if got == nil {
		t.Fatal("nilIfEmpty(\"   \"): want \"   \", got nil (whitespace is not empty)")
	}
	if s, ok := got.(string); !ok || s != "   " {
		t.Errorf("nilIfEmpty(\"   \"): got %v (%T)", got, got)
	}
}
