#!/usr/bin/env bash
# bundle-compile.sh — Compile workspace-configs-templates/ into .bundle.json artifacts.
#
# Usage:
#   ./bundle-compile.sh                     # compile all templates
#   ./bundle-compile.sh seo-agent           # compile one template
#   ./bundle-compile.sh --output-dir ./out  # custom output directory
#
# Each template folder becomes a self-contained .bundle.json that can be
# imported via POST /bundles/import.

set -euo pipefail

TEMPLATES_DIR="${TEMPLATES_DIR:-workspace-configs-templates}"
OUTPUT_DIR="."
SPECIFIC_TEMPLATE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
    --templates-dir) TEMPLATES_DIR="$2"; shift 2 ;;
    *) SPECIFIC_TEMPLATE="$1"; shift ;;
  esac
done

mkdir -p "$OUTPUT_DIR"

compile_template() {
  local dir="$1"
  local id
  id=$(basename "$dir")
  local config="$dir/config.yaml"

  if [[ ! -f "$config" ]]; then
    echo "  SKIP $id (no config.yaml)"
    return
  fi

  # Extract fields from config.yaml using python (portable, no yq dependency)
  local bundle
  bundle=$(python3 -c "
import json, yaml, os, sys
from pathlib import Path

dir = sys.argv[1]
config_path = os.path.join(dir, 'config.yaml')

with open(config_path) as f:
    config = yaml.safe_load(f)

bundle = {
    'schema': '1.0',
    'id': os.path.basename(dir),
    'name': config.get('name', ''),
    'description': config.get('description', ''),
    'tier': config.get('tier', 1),
    'model': config.get('model', ''),
    'system_prompt': '',
    'skills': [],
    'tools': [{'id': t, 'config': {}} for t in config.get('tools', [])],
    'prompts': {},
    'sub_workspaces': [],
    'agent_card': None,
    'author': '',
    'version': config.get('version', '1.0.0'),
}

# Load prompt files or system-prompt.md
prompt_files = config.get('prompt_files', ['system-prompt.md'])
prompts = []
for pf in prompt_files:
    fp = os.path.join(dir, pf)
    if os.path.exists(fp):
        prompts.append(Path(fp).read_text())
bundle['system_prompt'] = '\n\n'.join(prompts)

# Store config.yaml in prompts
bundle['prompts']['config.yaml'] = Path(config_path).read_text()

# Load skills
skills_dir = os.path.join(dir, 'skills')
if os.path.isdir(skills_dir):
    for skill_name in sorted(os.listdir(skills_dir)):
        skill_path = os.path.join(skills_dir, skill_name)
        if not os.path.isdir(skill_path):
            continue
        skill = {
            'id': skill_name,
            'name': skill_name,
            'description': '',
            'files': {},
        }
        for root, _, files in os.walk(skill_path):
            for fname in sorted(files):
                fpath = os.path.join(root, fname)
                rel = os.path.relpath(fpath, skill_path)
                try:
                    skill['files'][rel] = Path(fpath).read_text()
                except UnicodeDecodeError:
                    pass
        # Extract description from SKILL.md frontmatter
        skill_md = skill['files'].get('SKILL.md', '')
        if skill_md.startswith('---'):
            parts = skill_md.split('---', 2)
            if len(parts) >= 3:
                try:
                    fm = yaml.safe_load(parts[1])
                    skill['name'] = fm.get('name', skill_name)
                    skill['description'] = fm.get('description', '')
                except:
                    pass
        bundle['skills'].append(skill)

print(json.dumps(bundle, indent=2))
" "$dir")

  local outfile="$OUTPUT_DIR/${id}.bundle.json"
  echo "$bundle" > "$outfile"
  local size
  size=$(wc -c < "$outfile" | tr -d ' ')
  local skill_count
  skill_count=$(echo "$bundle" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['skills']))")
  echo "  OK $id → $outfile ($size bytes, $skill_count skills)"
}

echo "=== Bundle Compiler ==="
echo "Templates: $TEMPLATES_DIR"
echo "Output:    $OUTPUT_DIR"
echo ""

count=0
if [[ -n "$SPECIFIC_TEMPLATE" ]]; then
  dir="$TEMPLATES_DIR/$SPECIFIC_TEMPLATE"
  if [[ -d "$dir" ]]; then
    compile_template "$dir"
    count=1
  else
    echo "ERROR: Template not found: $dir"
    exit 1
  fi
else
  for dir in "$TEMPLATES_DIR"/*/; do
    [[ -d "$dir" ]] || continue
    compile_template "$dir"
    count=$((count + 1))
  done
fi

echo ""
echo "Compiled $count templates"
