package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// supportsRuntime tests — plugin runtime compatibility checking.

func TestSupportsRuntime_EmptyRuntimes(t *testing.T) {
	// Empty runtimes = unspecified, try it → always compatible.
	info := pluginInfo{Name: "test", Runtimes: nil}
	assert.True(t, info.supportsRuntime("claude_code"))
	assert.True(t, info.supportsRuntime("any_runtime"))
}

func TestSupportsRuntime_ExactMatch(t *testing.T) {
	info := pluginInfo{Name: "test", Runtimes: []string{"claude_code", "anthropic"}}
	assert.True(t, info.supportsRuntime("claude_code"))
	assert.True(t, info.supportsRuntime("anthropic"))
}

func TestSupportsRuntime_NoMatch(t *testing.T) {
	info := pluginInfo{Name: "test", Runtimes: []string{"claude_code"}}
	assert.False(t, info.supportsRuntime("openai"))
}

func TestSupportsRuntime_HyphenUnderscoreNormalized(t *testing.T) {
	// "claude-code" and "claude_code" are considered equal.
	info := pluginInfo{Name: "test", Runtimes: []string{"claude-code"}}
	assert.True(t, info.supportsRuntime("claude_code"))
	assert.True(t, info.supportsRuntime("claude-code")) // symmetric hyphen form
}

func TestSupportsRuntime_HyphenVsUnderscoreReverse(t *testing.T) {
	// Plugin declares underscore form; runtime uses hyphen.
	info := pluginInfo{Name: "test", Runtimes: []string{"claude_code"}}
	assert.True(t, info.supportsRuntime("claude-code"))
}

func TestSupportsRuntime_EmptyStringRuntime(t *testing.T) {
	info := pluginInfo{Name: "test", Runtimes: []string{"claude_code"}}
	// Empty runtime string: should not match any plugin.
	assert.False(t, info.supportsRuntime(""))
}

func TestSupportsRuntime_SingleRuntimeMatch(t *testing.T) {
	// Multiple declared runtimes: only matching one is sufficient.
	info := pluginInfo{Name: "test", Runtimes: []string{"python", "nodejs", "claude_code"}}
	assert.True(t, info.supportsRuntime("claude_code"))
	assert.False(t, info.supportsRuntime("ruby"))
}

func TestSupportsRuntime_AllHyphenForms(t *testing.T) {
	// Both plugin and runtime use hyphen form.
	info := pluginInfo{Name: "test", Runtimes: []string{"claude-code"}}
	assert.True(t, info.supportsRuntime("claude-code"))
}

func TestSupportsRuntime_MultipleHyphenNormalization(t *testing.T) {
	// Mixed hyphen/underscore forms normalize to the same.
	info := pluginInfo{Name: "test", Runtimes: []string{"some-runtime-name"}}
	assert.True(t, info.supportsRuntime("some_runtime_name"))
	assert.True(t, info.supportsRuntime("some-runtime-name"))
}

func TestSupportsRuntime_EmptyPluginRuntimesWithAnyInput(t *testing.T) {
	// Empty Runtimes on plugin = try it regardless of runtime.
	info := pluginInfo{Name: "test", Runtimes: []string{}}
	assert.True(t, info.supportsRuntime(""))
	assert.True(t, info.supportsRuntime("any"))
	assert.True(t, info.supportsRuntime("unknown"))
}

func TestSupportsRuntime_ZeroLengthRuntimes(t *testing.T) {
	// Empty slice vs nil: both should be treated as "unspecified".
	info := pluginInfo{Name: "test"}
	assert.True(t, info.supportsRuntime("anything"))
}
