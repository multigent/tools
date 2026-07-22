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
  --registry-dir /path/to/project/github-workflow-registry \
  --sync-interval 15m
```

Then create a custom MCP external tool in Multigent:

```text
Name: GitHub Workflow
MCP Server URL: http://127.0.0.1:39091/mcp
```

If Multigent API and this MCP server are on different machines, use an intranet
URL reachable by the API server and configure `--token`.

The MCP server owns GitHub metadata refresh. It starts a background sync on
startup and repeats it on `--sync-interval`; use `--sync-interval 0` to disable
that loop. Agents should not call sync directly. They read cached registry data
through inbox/show tools, and may call `github_workflow.sync_status` to check
freshness.
