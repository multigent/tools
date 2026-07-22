# Multigent Tools

External tools and adapters for Multigent.

This repository is intentionally separate from `multigent` so customer-specific
or ecosystem-specific tool servers can evolve without bloating the core agent
orchestration platform.

## Tools

- `github-workflow-mcp`: HTTP MCP wrapper around a `github-workflow` registry
  CLI for issue and PR workflow coordination.

## Build

```bash
go build ./cmd/github-workflow-mcp
```

## GitHub Workflow MCP

```bash
github-workflow-mcp \
  --addr 127.0.0.1:39091 \
  --github-workflow-bin /path/to/github-workflow \
  --registry-dir /path/to/project/github-workflow-registry
```

Then create a custom MCP external tool in Multigent:

```text
Name: GitHub Workflow
MCP Server URL: http://127.0.0.1:39091/mcp
```

If Multigent API and this MCP server are on different machines, use an intranet
URL reachable by the API server and configure `--token`.

`github_workflow.sync` starts a background refresh and returns immediately.
Use `github_workflow.sync_status` to check whether the last refresh is still
running, completed, or failed. This keeps agent wakeups from blocking on slow
GitHub metadata syncs.
