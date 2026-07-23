# Multigent Tools

External tool servers and adapters for [Multigent](https://github.com/multigent/multigent).

This repository is intentionally separate from the core product. Multigent should stay a coordination platform; ecosystem integrations that need background sync, custom binaries, or customer-specific infrastructure can live here as independent MCP servers.

## Available Tools

### `github-workflow-mcp`

An HTTP MCP server that exposes cached GitHub issue and pull request workflow data to Multigent agents.

It is useful when a team already maintains a local `github-workflow` registry and wants agents to read triage state, PR state, and sync freshness without running heavyweight sync commands during every agent wakeup.

## Build

```bash
go build -o dist/github-workflow-mcp ./cmd/github-workflow-mcp
```

## Run `github-workflow-mcp`

```bash
github-workflow-mcp \
  --addr 127.0.0.1:39091 \
  --github-workflow-bin /path/to/github-workflow \
  --registry-dir /path/to/project/github-workflow-registry \
  --sync-interval 15m
```

The server owns GitHub metadata refresh. It starts a background sync on startup and repeats it on `--sync-interval`; use `--sync-interval 0` to disable the loop.

Agents should not call sync directly. They read cached registry data through MCP tools and can call `github_workflow.sync_status` to check freshness.

## Connect From Multigent

Create an external tool in Multigent:

```text
Type: MCP Server
Name: GitHub Workflow
MCP Server URL: http://127.0.0.1:39091/mcp
```

If Multigent API and this MCP server are on different machines, use an intranet URL reachable by the API server and configure `--token`.

## Design Principle

Tool servers should provide a stable, narrow capability surface:

- own their background sync;
- expose read/write operations through MCP;
- avoid leaking host filesystem paths into agent prompts;
- keep credentials in the tool server or Multigent connection layer, not inside agent chat text.
