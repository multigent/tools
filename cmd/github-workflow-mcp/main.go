package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAddr         = "127.0.0.1:39091"
	defaultTimeout      = 10 * time.Minute
	defaultSyncInterval = 15 * time.Minute
	maxBodyBytes        = 1 << 20
)

type server struct {
	bin          string
	registryDir  string
	token        string
	timeout      time.Duration
	syncInterval time.Duration
	syncMu       sync.Mutex
	syncState    syncJobState
}

type syncJobState struct {
	Running    bool      `json:"running"`
	LastStart  time.Time `json:"lastStart,omitempty"`
	LastFinish time.Time `json:"lastFinish,omitempty"`
	LastOutput string    `json:"lastOutput,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolResult struct {
	Content []map[string]any `json:"content"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	addr := flag.String("addr", getenv("GITHUB_WORKFLOW_MCP_ADDR", defaultAddr), "HTTP listen address")
	bin := flag.String("github-workflow-bin", getenv("GITHUB_WORKFLOW_BIN", "github-workflow"), "github-workflow CLI binary")
	registryDir := flag.String("registry-dir", getenv("GITHUB_WORKFLOW_REGISTRY", ""), "github-workflow registry directory")
	token := flag.String("token", getenv("GITHUB_WORKFLOW_MCP_TOKEN", ""), "optional bearer token required from callers")
	timeoutText := flag.String("timeout", getenv("GITHUB_WORKFLOW_MCP_TIMEOUT", defaultTimeout.String()), "per command timeout")
	syncIntervalText := flag.String("sync-interval", getenv("GITHUB_WORKFLOW_MCP_SYNC_INTERVAL", defaultSyncInterval.String()), "background sync interval; set 0 to disable")
	flag.Parse()

	timeout, err := time.ParseDuration(*timeoutText)
	if err != nil {
		log.Fatalf("invalid --timeout: %v", err)
	}
	syncInterval, err := time.ParseDuration(*syncIntervalText)
	if err != nil {
		log.Fatalf("invalid --sync-interval: %v", err)
	}
	if strings.TrimSpace(*registryDir) == "" {
		log.Fatalf("--registry-dir is required")
	}
	s := &server{
		bin:          strings.TrimSpace(*bin),
		registryDir:  strings.TrimSpace(*registryDir),
		token:        strings.TrimSpace(*token),
		timeout:      timeout,
		syncInterval: syncInterval,
	}
	s.startBackgroundSync()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/health", s.handleHealth)
	log.Printf("github-workflow MCP listening on http://%s/mcp registry=%s", *addr, s.registryDir)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		writeRPCError(w, nil, -32001, "unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeRPCError(w, nil, -32700, "read request failed")
		return
	}
	if len(body) > maxBodyBytes {
		writeRPCError(w, nil, -32600, "request too large")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "invalid JSON-RPC request")
		return
	}
	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]any{"name": "github-workflow-mcp", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": tools()})
	case "tools/call":
		s.handleToolCall(w, r, req)
	default:
		writeRPCError(w, req.ID, -32601, "method not found")
	}
}

func (s *server) handleToolCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params toolCallParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeRPCError(w, req.ID, -32602, "invalid tools/call params")
			return
		}
	}
	out, err := s.callTool(r.Context(), strings.TrimSpace(params.Name), params.Arguments)
	if err != nil {
		writeRPCError(w, req.ID, -32000, err.Error())
		return
	}
	writeRPCResult(w, req.ID, toolResult{Content: []map[string]any{{"type": "text", "text": out}}})
}

func (s *server) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case "github_workflow.sync_status":
		return s.syncStatus()
	case "github_workflow.issue_inbox":
		return s.run(ctx, "issue", "inbox", "--view", stringArg(args, "view", "pm"))
	case "github_workflow.issue_show":
		number, err := intArg(args, "number")
		if err != nil {
			return "", err
		}
		return s.run(ctx, "issue", "show", "--number", strconv.Itoa(number))
	case "github_workflow.issue_set":
		cliArgs, err := issueSetArgs(args)
		if err != nil {
			return "", err
		}
		return s.run(ctx, append([]string{"issue", "set"}, cliArgs...)...)
	case "github_workflow.pr_inbox":
		return s.run(ctx, "pr", "inbox", "--view", stringArg(args, "view", "qa"))
	case "github_workflow.pr_show":
		number, err := intArg(args, "number")
		if err != nil {
			return "", err
		}
		return s.run(ctx, "pr", "show", "--number", strconv.Itoa(number))
	case "github_workflow.pr_set":
		cliArgs, err := prSetArgs(args)
		if err != nil {
			return "", err
		}
		return s.run(ctx, append([]string{"pr", "set"}, cliArgs...)...)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (s *server) startBackgroundSync() {
	if s.syncInterval <= 0 {
		log.Printf("github-workflow background sync disabled")
		return
	}
	s.startSync("startup")
	go func() {
		ticker := time.NewTicker(s.syncInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.startSync("scheduled")
		}
	}()
}

func (s *server) startSync(reason string) {
	s.syncMu.Lock()
	if s.syncState.Running {
		s.syncMu.Unlock()
		log.Printf("github-workflow sync skipped: already running")
		return
	}
	s.syncState.Running = true
	s.syncState.LastStart = time.Now()
	s.syncState.LastFinish = time.Time{}
	s.syncState.LastOutput = ""
	s.syncState.LastError = ""
	s.syncMu.Unlock()

	go func() {
		log.Printf("github-workflow sync started reason=%s", reason)
		out, err := s.run(context.Background(), "sync")
		if err == nil {
			if renderOut, renderErr := s.run(context.Background(), "render", "--all"); renderErr != nil {
				err = fmt.Errorf("render after sync failed: %w", renderErr)
			} else if strings.TrimSpace(renderOut) != "" {
				out = strings.TrimSpace(out + "\n" + renderOut)
			}
		}
		s.syncMu.Lock()
		defer s.syncMu.Unlock()
		s.syncState.Running = false
		s.syncState.LastFinish = time.Now()
		s.syncState.LastOutput = out
		if err != nil {
			s.syncState.LastError = err.Error()
			log.Printf("github-workflow sync failed: %v", err)
			return
		}
		log.Printf("github-workflow sync completed")
	}()
}

func (s *server) syncStatus() (string, error) {
	s.syncMu.Lock()
	state := s.syncState
	s.syncMu.Unlock()
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (s *server) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	fullArgs := append([]string{"--registry-dir", s.registryDir}, args...)
	cmd := exec.CommandContext(ctx, s.bin, fullArgs...)
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	errOut := strings.TrimSpace(stderr.String())
	if ctx.Err() != nil {
		return out, fmt.Errorf("github-workflow timed out after %s", s.timeout)
	}
	if err != nil {
		if errOut != "" {
			return out, fmt.Errorf("%s", errOut)
		}
		return out, err
	}
	if errOut != "" {
		if out == "" {
			return errOut, nil
		}
		return out + "\n\nstderr:\n" + errOut, nil
	}
	return out, nil
}

func issueSetArgs(args map[string]any) ([]string, error) {
	number, err := intArg(args, "number")
	if err != nil {
		return nil, err
	}
	out := []string{"--number", strconv.Itoa(number)}
	addStringFlag(&out, args, "workflow_state", "--workflow-state")
	addStringFlag(&out, args, "triage", "--triage")
	addIntFlag(&out, args, "priority", "--priority")
	addStringFlag(&out, args, "owner_agent", "--owner-agent")
	addStringFlag(&out, args, "assigned_agent", "--assigned-agent")
	addStringFlag(&out, args, "waiting_on", "--waiting-on")
	addCSVFlag(&out, args, "linked_prs", "--linked-prs")
	addCSVFlag(&out, args, "linked_tasks", "--linked-tasks")
	addStringFlag(&out, args, "notes", "--notes")
	if boolArg(args, "allow_registry_only") {
		out = append(out, "--allow-registry-only")
	}
	return out, nil
}

func prSetArgs(args map[string]any) ([]string, error) {
	number, err := intArg(args, "number")
	if err != nil {
		return nil, err
	}
	out := []string{"--number", strconv.Itoa(number)}
	addStringFlag(&out, args, "workflow_state", "--workflow-state")
	addStringFlag(&out, args, "assigned_reviewer", "--assigned-reviewer")
	addStringFlag(&out, args, "waiting_on", "--waiting-on")
	if _, ok := args["merge_ready"]; ok && boolArg(args, "merge_ready") {
		out = append(out, "--merge-ready")
	}
	addCSVFlag(&out, args, "linked_issues", "--linked-issues")
	addCSVFlag(&out, args, "linked_tasks", "--linked-tasks")
	addStringFlag(&out, args, "notes", "--notes")
	return out, nil
}

func tools() []toolDef {
	return []toolDef{
		{Name: "github_workflow.sync_status", Description: "Show the current or last GitHub workflow sync status.", InputSchema: objectSchema(nil, nil)},
		{Name: "github_workflow.issue_inbox", Description: "List issues for a workflow view. PM should normally use view=pm.", InputSchema: objectSchema(map[string]any{"view": enumStringSchema("Issue view.", []string{"pm", "unanswered", "false-complete", "all", "new", "needs-info", "waiting-human", "has-pr", "assigned", "release-blocker", "stale"})}, nil)},
		{Name: "github_workflow.issue_show", Description: "Show one tracked issue record as JSON.", InputSchema: objectSchema(map[string]any{"number": integerSchema("GitHub issue number.")}, []string{"number"})},
		{Name: "github_workflow.issue_set", Description: "Update issue workflow fields in the registry. Normal PM triage should comment on GitHub before marking progress.", InputSchema: objectSchema(map[string]any{"number": integerSchema("GitHub issue number."), "workflow_state": stringSchema("Workflow state, for example assigned, needs-info, has-pr, waiting-human."), "triage": stringSchema("Triage category, for example bug-dev, answer-now, needs-repro, has-pr, owner-decision."), "priority": integerSchema("Priority 0-3."), "owner_agent": stringSchema("Owning PM/support agent id."), "assigned_agent": stringSchema("Assigned downstream agent id."), "waiting_on": stringSchema("Who the issue is waiting on: user, dev, qa, human, none."), "linked_prs": listSchema("Linked PR numbers."), "linked_tasks": listSchema("Linked Multigent task ids."), "notes": stringSchema("Short workflow notes."), "allow_registry_only": boolSchema("Override the GitHub reply guard. Use only for exceptional registry maintenance.")}, []string{"number"})},
		{Name: "github_workflow.pr_inbox", Description: "List PRs for a workflow view. QA should normally use view=qa.", InputSchema: objectSchema(map[string]any{"view": enumStringSchema("PR view.", []string{"pm", "all", "qa", "needs-review", "ci-failing", "ready", "awaiting-author", "stale"})}, nil)},
		{Name: "github_workflow.pr_show", Description: "Show one tracked PR record as JSON.", InputSchema: objectSchema(map[string]any{"number": integerSchema("GitHub PR number.")}, []string{"number"})},
		{Name: "github_workflow.pr_set", Description: "Update PR workflow fields in the registry after GitHub review or release triage.", InputSchema: objectSchema(map[string]any{"number": integerSchema("GitHub PR number."), "workflow_state": stringSchema("Workflow state, for example ready-to-merge, awaiting-author, blocked."), "assigned_reviewer": stringSchema("Assigned QA/reviewer agent id."), "waiting_on": stringSchema("Who the PR is waiting on: dev, qa, human, none."), "merge_ready": boolSchema("Mark the PR as merge ready."), "linked_issues": listSchema("Linked issue numbers."), "linked_tasks": listSchema("Linked Multigent task ids."), "notes": stringSchema("Short workflow notes.")}, []string{"number"})},
	}
}

func (s server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	return strings.TrimSpace(r.Header.Get("Authorization")) == "Bearer "+s.token
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(defaultID(id)), "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(defaultID(id)), "error": rpcError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func defaultID(id json.RawMessage) []byte {
	if len(id) == 0 {
		return []byte("null")
	}
	return id
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func stringArg(args map[string]any, key, fallback string) string {
	value, ok := args[key]
	if !ok {
		return fallback
	}
	if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return fallback
}

func intArg(args map[string]any, key string) (int, error) {
	value, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	switch v := value.(type) {
	case float64:
		return int(v), nil
	case int:
		return v, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
}

func boolArg(args map[string]any, key string) bool {
	v, ok := args[key]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "1" || b == "yes"
	default:
		return false
	}
}

func addStringFlag(out *[]string, args map[string]any, key, flag string) {
	if value := stringArg(args, key, ""); value != "" {
		*out = append(*out, flag, value)
	}
}

func addIntFlag(out *[]string, args map[string]any, key, flag string) {
	if _, ok := args[key]; !ok {
		return
	}
	n, err := intArg(args, key)
	if err == nil {
		*out = append(*out, flag, strconv.Itoa(n))
	}
}

func addCSVFlag(out *[]string, args map[string]any, key, flag string) {
	values, ok := csvArg(args[key])
	if ok && values != "" {
		*out = append(*out, flag, values)
	}
}

func csvArg(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return strings.TrimSpace(v), strings.TrimSpace(v) != ""
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch x := item.(type) {
			case string:
				if strings.TrimSpace(x) != "" {
					parts = append(parts, strings.TrimSpace(x))
				}
			case float64:
				parts = append(parts, strconv.Itoa(int(x)))
			}
		}
		return strings.Join(parts, ","), len(parts) > 0
	default:
		return "", false
	}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enumStringSchema(description string, values []string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func integerSchema(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func boolSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func listSchema(description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": map[string]any{"type": "string"}}
}
