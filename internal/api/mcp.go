package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/champion19007/agentd/internal/core/domain"
)

// MCP Protocol constants
const (
	mcpProtocolVersion = "2024-11-05"
	mcpJSONRPCVersion  = "2.0"
)

// MCPServer exposes Agentd's read-oriented state through the Model Context Protocol.
// External models and AI agents can query checks, runs, incidents, and status.
//
// Architectural Invariant:
// MCPServer exposes ONLY read operations. It NEVER exposes arbitrary write actions
// or repair approvals to external agents. Human approval must remain human approval.
type MCPServer struct {
	ops Operations
}

// NewMCPServer constructs an MCPServer backed by application Operations.
func NewMCPServer(ops Operations) *MCPServer {
	return &MCPServer{ops: ops}
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id,omitempty"`
	Result  any           `json:"result,omitempty"`
	Error   *mcpErrorData `json:"error,omitempty"`
}

type mcpErrorData struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpCallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type mcpToolResult struct {
	Content []mcpContentBlock `json:"content"`
	IsError bool              `json:"isError"`
}

type mcpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ServeStdio serves JSON-RPC MCP requests over standard I/O (stdin -> stdout).
func (s *MCPServer) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// Support large payloads (up to 4MB)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 {
			continue
		}

		respBytes, err := s.HandleMessage(ctx, []byte(line))
		if err != nil {
			return err
		}
		if len(respBytes) > 0 {
			if _, err := out.Write(append(respBytes, '\n')); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// HandleMessage processes a single JSON-RPC MCP message and returns the response.
func (s *MCPServer) HandleMessage(ctx context.Context, raw []byte) ([]byte, error) {
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return json.Marshal(mcpResponse{
			JSONRPC: mcpJSONRPCVersion,
			Error:   &mcpErrorData{Code: -32700, Message: "Parse error: invalid JSON"},
		})
	}

	// Notifications have no ID and expect no response
	isNotification := req.ID == nil

	switch req.Method {
	case "initialize":
		resp := mcpResponse{
			JSONRPC: mcpJSONRPCVersion,
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "agentd",
					"version": "1.0.0",
				},
			},
		}
		return json.Marshal(resp)

	case "notifications/initialized":
		return nil, nil

	case "ping":
		if isNotification {
			return nil, nil
		}
		return json.Marshal(mcpResponse{
			JSONRPC: mcpJSONRPCVersion,
			ID:      req.ID,
			Result:  map[string]any{},
		})

	case "tools/list":
		tools := s.listTools()
		return json.Marshal(mcpResponse{
			JSONRPC: mcpJSONRPCVersion,
			ID:      req.ID,
			Result: map[string]any{
				"tools": tools,
			},
		})

	case "tools/call":
		var params mcpCallToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return json.Marshal(mcpResponse{
				JSONRPC: mcpJSONRPCVersion,
				ID:      req.ID,
				Error:   &mcpErrorData{Code: -32602, Message: "Invalid params"},
			})
		}

		result, err := s.callTool(ctx, params.Name, params.Arguments)
		if err != nil {
			return json.Marshal(mcpResponse{
				JSONRPC: mcpJSONRPCVersion,
				ID:      req.ID,
				Result: mcpToolResult{
					IsError: true,
					Content: []mcpContentBlock{{Type: "text", Text: err.Error()}},
				},
			})
		}

		return json.Marshal(mcpResponse{
			JSONRPC: mcpJSONRPCVersion,
			ID:      req.ID,
			Result:  result,
		})

	default:
		if isNotification {
			return nil, nil
		}
		return json.Marshal(mcpResponse{
			JSONRPC: mcpJSONRPCVersion,
			ID:      req.ID,
			Error:   &mcpErrorData{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
		})
	}
}

func (s *MCPServer) listTools() []mcpToolDefinition {
	return []mcpToolDefinition{
		{
			Name:        "list_checks",
			Description: "List all configured and active checks being monitored by Agentd.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "inspect_check",
			Description: "Get detailed information about a specific check, including intent, schedule, active binding, and recent runs.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"check_id": map[string]any{
						"type":        "string",
						"description": "The check ID to inspect (e.g. check-pricing)",
					},
				},
				"required": []string{"check_id"},
			},
		},
		{
			Name:        "list_runs",
			Description: "List recent execution runs for a check.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"check_id": map[string]any{
						"type":        "string",
						"description": "The check ID whose runs to list",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of runs to return (default 20)",
					},
				},
				"required": []string{"check_id"},
			},
		},
		{
			Name:        "inspect_run",
			Description: "Get detailed outcome, timestamps, and payload hash of a specific check run.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id": map[string]any{
						"type":        "string",
						"description": "The run ID to inspect",
					},
				},
				"required": []string{"run_id"},
			},
		},
		{
			Name:        "list_incidents",
			Description: "List all open breakage incidents requiring operator review.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "inspect_incident",
			Description: "Get comprehensive incident details including breakage explanation, proposed repair binding, diff, and verification results.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"incident_id": map[string]any{
						"type":        "string",
						"description": "The incident ID to inspect",
					},
				},
				"required": []string{"incident_id"},
			},
		},
		{
			Name:        "inspect_status",
			Description: "Get the overall health, database integrity, schema version, check counts, and open incidents of the Agentd daemon.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func (s *MCPServer) callTool(ctx context.Context, name string, args map[string]any) (mcpToolResult, error) {
	switch name {
	case "list_checks":
		checks, err := s.ops.ListChecks(ctx)
		if err != nil {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: err.Error()}}}, nil
		}
		return formatToolJSON(checks)

	case "inspect_check":
		checkID, _ := args["check_id"].(string)
		if checkID == "" {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: "argument 'check_id' is required"}}}, nil
		}
		detail, err := s.ops.GetCheck(ctx, domain.CheckID(checkID))
		if err != nil {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: err.Error()}}}, nil
		}
		return formatToolJSON(detail)

	case "list_runs":
		checkID, _ := args["check_id"].(string)
		if checkID == "" {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: "argument 'check_id' is required"}}}, nil
		}
		limit := 20
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		runs, err := s.ops.ListRuns(ctx, domain.CheckID(checkID), limit)
		if err != nil {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: err.Error()}}}, nil
		}
		return formatToolJSON(runs)

	case "inspect_run":
		runID, _ := args["run_id"].(string)
		if runID == "" {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: "argument 'run_id' is required"}}}, nil
		}
		runDetail, err := s.ops.GetRun(ctx, domain.RunID(runID))
		if err != nil {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: err.Error()}}}, nil
		}
		return formatToolJSON(runDetail)

	case "list_incidents":
		incidents, err := s.ops.ListIncidents(ctx)
		if err != nil {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: err.Error()}}}, nil
		}
		return formatToolJSON(incidents)

	case "inspect_incident":
		incidentID, _ := args["incident_id"].(string)
		if incidentID == "" {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: "argument 'incident_id' is required"}}}, nil
		}
		incDetail, err := s.ops.GetIncident(ctx, domain.IncidentID(incidentID))
		if err != nil {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: err.Error()}}}, nil
		}
		return formatToolJSON(incDetail)

	case "inspect_status", "current_status":
		status, err := s.ops.Status(ctx)
		if err != nil {
			return mcpToolResult{IsError: true, Content: []mcpContentBlock{{Type: "text", Text: err.Error()}}}, nil
		}
		return formatToolJSON(status)

	// Explicitly reject write operations through MCP
	case "approve_repair", "reject_repair", "add_check", "delete_check", "run_check", "gc", "backup":
		return mcpToolResult{
			IsError: true,
			Content: []mcpContentBlock{
				{
					Type: "text",
					Text: "security violation: external agents cannot perform write actions or approve repairs via MCP; human approval must remain human approval",
				},
			},
		}, nil

	default:
		return mcpToolResult{
			IsError: true,
			Content: []mcpContentBlock{
				{
					Type: "text",
					Text: fmt.Sprintf("unknown MCP tool %q", name),
				},
			},
		}, nil
	}
}

func formatToolJSON(data any) (mcpToolResult, error) {
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return mcpToolResult{}, err
	}
	return mcpToolResult{
		IsError: false,
		Content: []mcpContentBlock{
			{
				Type: "text",
				Text: string(bytes),
			},
		},
	}, nil
}
