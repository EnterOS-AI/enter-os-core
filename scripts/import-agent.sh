#!/usr/bin/env bash
# import-agent.sh — Import any agent folder into Molecule AI as a workspace template.
#
# Usage:
#   ./import-agent.sh ~/path/to/my-openclaw-agent
#   ./import-agent.sh ~/path/to/my-openclaw-agent my-custom-name
#
# Supports: OpenClaw, Claude Code, Codex, or any folder with .md files + skills/

set -euo pipefail

SOURCE="${1:?Usage: ./import-agent.sh <agent-folder> [template-name]}"
TEMPLATES_DIR="workspace-configs-templates"

# Derive template name from folder name or second argument
TEMPLATE_NAME="${2:-$(basename "$SOURCE")}"
# Normalize: lowercase, replace spaces with hyphens
TEMPLATE_NAME=$(echo "$TEMPLATE_NAME" | tr '[:upper:]' '[:lower:]' | tr ' ' '-')
DEST="$TEMPLATES_DIR/$TEMPLATE_NAME"

if [[ -d "$DEST" ]]; then
  echo "Template '$TEMPLATE_NAME' already exists at $DEST"
  echo "Delete it first or choose a different name."
  exit 1
fi

echo "=== Importing Agent ==="
echo "Source:   $SOURCE"
echo "Template: $TEMPLATE_NAME"
echo "Dest:     $DEST"
echo ""

# Copy the folder
cp -r "$SOURCE" "$DEST"

# Detect what kind of agent this is by looking at files
PROMPT_FILES=()
for f in SOUL.md BOOTSTRAP.md AGENTS.md HEARTBEAT.md TOOLS.md USER.md IDENTITY.md MEMORY.md; do
  [[ -f "$DEST/$f" ]] && PROMPT_FILES+=("$f")
done

# Claude Code style
[[ -f "$DEST/CLAUDE.md" ]] && PROMPT_FILES+=("CLAUDE.md")

# Codex style
if [[ ${#PROMPT_FILES[@]} -eq 0 && -f "$DEST/AGENTS.md" ]]; then
  PROMPT_FILES+=("AGENTS.md")
fi

# Fallback: system-prompt.md
if [[ ${#PROMPT_FILES[@]} -eq 0 ]]; then
  if [[ -f "$DEST/system-prompt.md" ]]; then
    PROMPT_FILES+=("system-prompt.md")
  else
    # Use any .md file in root
    for f in "$DEST"/*.md; do
      [[ -f "$f" ]] && PROMPT_FILES+=("$(basename "$f")")
    done
  fi
fi

# Detect skills
SKILLS=()
if [[ -d "$DEST/skills" ]]; then
  for d in "$DEST/skills"/*/; do
    [[ -d "$d" ]] && SKILLS+=("$(basename "$d")")
  done
fi

# Build prompt_files YAML list
PROMPT_YAML=""
for f in "${PROMPT_FILES[@]}"; do
  PROMPT_YAML+="  - $f\n"
done

# Build skills YAML list
SKILLS_YAML=""
for s in "${SKILLS[@]}"; do
  SKILLS_YAML+="  - $s\n"
done

# Generate config.yaml if it doesn't exist
if [[ ! -f "$DEST/config.yaml" ]]; then
  DISPLAY_NAME=$(echo "$TEMPLATE_NAME" | sed 's/-/ /g' | awk '{for(i=1;i<=NF;i++) $i=toupper(substr($i,1,1)) substr($i,2)}1')

  cat > "$DEST/config.yaml" << EOF
name: $DISPLAY_NAME
description: Imported from $(basename "$SOURCE")
version: 1.0.0
tier: 1

model: anthropic:claude-haiku-4-5-20251001

prompt_files:
$(echo -e "$PROMPT_YAML")
skills:
$(echo -e "${SKILLS_YAML:-  []}")
tools: []

a2a:
  port: 8000
  streaming: true
  push_notifications: true

delegation:
  retry_attempts: 3
  retry_delay: 5
  timeout: 120
  escalate: true

sub_workspaces: []

env:
  required:
    - ANTHROPIC_API_KEY
  optional: []
EOF
  echo "Generated config.yaml"
else
  echo "config.yaml already exists, keeping it"
fi

echo ""
echo "=== Import Complete ==="
echo ""
echo "Prompt files: ${PROMPT_FILES[*]}"
echo "Skills:       ${SKILLS[*]:-none}"
echo ""
echo "To deploy:"
echo "  1. Open http://localhost:3000"
echo "  2. Click the template palette (grid icon, top-left)"
echo "  3. Click '$DISPLAY_NAME' to deploy"
echo ""
echo "Or set API key first:"
echo "  1. Deploy the workspace"
echo "  2. Click it → Settings tab → set ANTHROPIC_API_KEY"
echo "  3. Click Details tab → Restart"
