# Bundle System

A workspace bundle is the portable unit of the platform. It is a single `.bundle.json` file that captures everything needed to recreate a workspace anywhere.

## Bundle Format

```json
{
  "schema": "1.0",
  "id": "seo-agent-vancouver",
  "name": "Vancouver SEO Agent",
  "description": "Bilingual EN/ZH SEO page generator",
  "tier": 1,
  "model": "anthropic:claude-sonnet-4-6",
  "system_prompt": "...full prompt...",
  "skills": [
    {
      "id": "generate-seo-page",
      "name": "Generate SEO Landing Page",
      "description": "...",
      "files": {
        "SKILL.md": "---\nname: Generate SEO Landing Page\ndescription: ...\n---\n\nInstructions for the agent...",
        "tools/write_page.py": "def write_page(keyword, lang):\n    ...",
        "tools/check_gsc.py": "def check_gsc(url):\n    ..."
      }
    }
  ],
  "tools": [
    { "id": "web_search", "config": {} },
    { "id": "gsc_api", "config": { "scopes": ["search-console"] } }
  ],
  "prompts": {
    "prompts/page-generation.md": "...full content...",
    "templates/renovation-page.html": "...full content..."
  },
  "sub_workspaces": [],
  "agent_card": { "...": "A2A card snapshot" },
  "author": "your-name",
  "version": "1.2.0"
}
```

## Skill Serialization

Each skill folder is serialized into a `files` dict — every file in the skill folder becomes a key (relative path) with its content as the value. No special treatment for any file type — `SKILL.md`, tool scripts, templates, and any other files are all serialized the same way.

```python
def serialize_skill(skill_path: Path) -> dict:
    skill_data = {"id": skill_path.name, "files": {}}
    for file in skill_path.rglob("*"):
        if file.is_file():
            rel = str(file.relative_to(skill_path))
            skill_data["files"][rel] = file.read_text()
    # name/description extracted from SKILL.md frontmatter
    md = skill_data["files"].get("SKILL.md", "")
    skill_data["name"] = extract_frontmatter(md, "name")
    skill_data["description"] = extract_frontmatter(md, "description")
    return skill_data
```

On import, the importer reverses the process — it writes each key back as a file under the skill folder:

```python
def deserialize_skill(skill_data: dict, target_path: Path):
    skill_dir = target_path / skill_data["id"]
    for rel_path, content in skill_data["files"].items():
        file_path = skill_dir / rel_path
        file_path.parent.mkdir(parents=True, exist_ok=True)
        file_path.write_text(content)
```

## What Is Included

- The full system prompt text
- All skill files (every file in each skill folder, inlined as strings in a `files` dict)
- All prompt templates (markdown files, inlined)
- All asset files (HTML templates, etc., inlined)
- Tool configurations (which tools to use, how configured)
- Sub-workspace bundles recursively (for team workspaces)
- A snapshot of the Agent Card

## What Is NOT Included

- **API keys or secrets** — buyer brings their own
- **Memory or conversation history** — buyer starts fresh
- **Database data** — not portable

## Recursive Sub-Workspaces

`sub_workspaces` is an array of nested bundles. A team workspace contains the full bundles of all its members. The importer walks the tree recursively and provisions each workspace it finds.

## How Bundles Are Built

The `workspace-configs-templates/` folder contains the source files for each workspace type. The `bundle-compile.sh` script walks each folder and inlines all files into a single `workspace.bundle.json`. This is the compiled artifact — like a built binary from source files.

## Import/Export Workflow

### Export
1. Right-click node on canvas
2. Select "Export as bundle"
3. Platform serializes the running workspace via `GET /bundles/export/:id`
4. Downloads `seo-agent.bundle.json`

### Import
1. Drag `.bundle.json` onto canvas
2. Canvas sends `POST /bundles/import`
3. Platform importer (`bundle/importer.go`) walks `sub_workspaces` recursively
4. Each workspace gets a container provisioned with extracted config
5. New nodes appear on canvas

### Partial Import Failure

If a bundle contains sub-workspaces and one fails to provision:
1. Successfully provisioned workspaces remain running
2. Failed workspaces get `WORKSPACE_PROVISION_FAILED` events
3. The parent workspace still comes online (it can operate with fewer sub-workspaces)
4. Canvas shows failed sub-workspace nodes in red with retry buttons
5. User can retry individual failed sub-workspaces without re-importing the whole bundle

### ID Generation on Import

**The platform always generates fresh workspace IDs on import.** The original bundle `id` is stored as `source_bundle_id` in the workspace record for traceability.

This means:
- You can import the same bundle twice to run two instances
- The bundle `id` is the **template identity**, the workspace `id` is the **instance identity**
- You can trace which template a workspace came from
- You can find all instances of the same bundle
- You can push updates from the original template

### Duplicate
Export + re-import with new IDs (always — no overwriting).

## Future: Marketplace

Bundles are the unit of sale:

- A seller lists a bundle with a price
- A buyer purchases it
- The platform provisions it in the buyer's environment with their own API keys
- The seller's prompts and skills are included
- Like buying a Shopify theme — you get the design and logic, not the store's data

## Related Docs

- [Workspace Runtime](./workspace-runtime.md) — Source files that compile into bundles
- [Platform API](../api-protocol/platform-api.md) — Import/export endpoints
- [Canvas UI](../frontend/canvas.md) — Drag-and-drop bundle interactions
