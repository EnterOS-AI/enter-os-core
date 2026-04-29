"""Platform tools — single source of truth for tool naming and docs.

The platform owns A2A and persistent-memory tooling (cross-cutting
runtime concerns per project memory project_runtime_native_pluggable.md).
Tools are defined ONCE in `registry.py`. Every adapter — MCP server,
LangChain wrapper, any future SDK integration — consumes the specs to
register the tool in its native format. Doc generators (system-prompt
injection, canvas help, future doc sites) read from the same place.

Adding a tool: append a ToolSpec to TOOLS in registry.py. Every
adapter picks it up automatically; structural tests fail if any side
drifts from the registry.
"""
