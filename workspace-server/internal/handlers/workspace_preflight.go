package handlers

import (
	"fmt"
	"log"
	"strings"

	"gopkg.in/yaml.v3"
)

// requiredEnvSchema is the subset of config.yaml we read to decide which env
// vars must be present before a container launch. It maps the YAML path
// `runtime_config.required_env: [...]` which is the same shape the workspace
// adapter's preflight reads inside the container (workspace/preflight.py).
//
// Mirroring the check server-side lets us fail fast with a readable error
// instead of letting the container crash-loop and the workspace sit in
// `provisioning` until a sweeper or the user intervenes.
type requiredEnvSchema struct {
	RuntimeConfig struct {
		RequiredEnv []string `yaml:"required_env"`
	} `yaml:"runtime_config"`
}

// missingRequiredEnv returns the list of env var names declared in the
// workspace's config.yaml under `runtime_config.required_env` that are NOT
// present (or are empty) in the assembled envVars map. Returns an empty
// slice when the config declares no requirements or when all are satisfied.
//
// A parse failure returns no missing vars — config.yaml shape is enforced by
// the in-container preflight, and the server's job here is only to catch the
// common "forgot to add the OAuth token secret" footgun, not to be a second
// config validator.
func missingRequiredEnv(configFiles map[string][]byte, envVars map[string]string) []string {
	if len(configFiles) == 0 {
		return nil
	}
	raw, ok := configFiles["config.yaml"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var schema requiredEnvSchema
	if err := yaml.Unmarshal(raw, &schema); err != nil {
		// Safe default: the in-container preflight is the source of truth
		// for config.yaml shape, so we don't block the provision here. But
		// log at WARN so operators can notice a template with malformed
		// YAML — otherwise a silently-skipped preflight is invisible.
		log.Printf("Preflight: WARN — config.yaml unparseable, skipping required_env check: %v", err)
		return nil
	}
	if len(schema.RuntimeConfig.RequiredEnv) == 0 {
		return nil
	}
	var missing []string
	for _, name := range schema.RuntimeConfig.RequiredEnv {
		if v, ok := envVars[name]; !ok || v == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// formatMissingEnvError builds the user-facing message for a provision
// failure caused by unset required env vars. Kept stable because it's
// rendered verbatim in the canvas Events tab and Details banner.
func formatMissingEnvError(missing []string) string {
	if len(missing) == 1 {
		return fmt.Sprintf(
			"missing required env var %q — add it under Config → Env Vars (or as a Global secret) and retry",
			missing[0],
		)
	}
	return fmt.Sprintf(
		"missing required env vars %s — add them under Config → Env Vars (or as Global secrets) and retry",
		strings.Join(missing, ", "),
	)
}

// formatMissingBYOKCredentialError builds the user-facing message for a
// provision failure caused by a non-platform (byok/subscription) workspace
// that has no usable LLM credential of its own (internal#711). The platform's
// scope:global LLM credentials are NOT a valid fallback for a non-platform
// workspace — resolving to them would bill the platform's Anthropic credits —
// so the provision fails closed here rather than starting the workspace on
// stripped/absent creds. Rendered verbatim in the canvas Events tab.
func formatMissingBYOKCredentialError(mode string) string {
	return fmt.Sprintf(
		"this workspace's LLM billing mode is %q (not platform-managed) but it has no LLM credential of its own. "+
			"Add a workspace-scoped credential (e.g. CLAUDE_CODE_OAUTH_TOKEN or your provider's API key) under "+
			"Config → Secrets, or switch the workspace to platform-managed billing via "+
			"/admin/workspaces/:id/llm-billing-mode, then retry. The platform's shared LLM credentials are not "+
			"used for non-platform workspaces.",
		mode,
	)
}

// formatMissingPlatformProxyError builds the user-facing message for a
// provision failure caused by a platform-managed workspace whose control-plane
// proxy environment is absent (#2162). The platform-managed path requires
// MOLECULE_LLM_BASE_URL + MOLECULE_LLM_USAGE_TOKEN (or their OPENAI_*
// fallbacks) to inject a usable credential; without them the workspace must
// NOT start credential-less.
func formatMissingPlatformProxyError() string {
	return "this workspace is configured for platform-managed LLM billing but the control-plane proxy is not ready. " +
		"The required platform proxy env (MOLECULE_LLM_BASE_URL + MOLECULE_LLM_USAGE_TOKEN) is absent. " +
		"This is usually a transient boot-race; retry in 30 seconds. If it persists, verify the platform proxy " +
		"is configured for this tenant/runtime and contact the platform team."
}

// formatMissingModelError builds the user-facing message for a provision that
// reaches launch with no resolved model (core#2594). The platform no longer
// substitutes a default model (the MOLECULE_LLM_DEFAULT_MODEL env fail-open was
// removed); a model is required input. Rather than letting the runtime fall back
// to its hardcoded anthropic:claude-opus-4-7, the provision fails closed here.
func formatMissingModelError(mode string) string {
	return fmt.Sprintf(
		"this workspace (LLM billing mode %q) reached provisioning with no model set. "+
			"The platform does not pick a default model — choose one under Config → Model "+
			"(or set the MODEL workspace secret) and retry. Provisioning is refused rather "+
			"than silently running an arbitrary model.",
		mode,
	)
}
