// Package plugins is a driven adapter hosting MCP plugins over stdio.
//
// A plugin is a separate executable Agentd starts and talks JSON-RPC to over
// its stdin and stdout. That is a deliberately unfashionable choice and it buys
// three things worth having:
//
//   - A plugin cannot corrupt Agentd. It is a process, not a Go plugin loaded
//     into this address space, so a crash in someone's scraper is a failed
//     check rather than a failed daemon.
//   - A plugin can be written in anything.
//   - A plugin can be killed. A context deadline becomes a dead process, which
//     is not true of a goroutine that has stopped cooperating.
//
// The cost is that the environment a plugin inherits matters. Agentd passes
// only what a plugin is configured to need, because a plugin that inherits the
// parent environment inherits every secret in it.
package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Protocol constants.
const (
	protocolVersion = "2024-11-05"
	jsonRPCVersion  = "2.0"
)

// DefaultTimeout bounds one plugin call.
const DefaultTimeout = 60 * time.Second

// Spec describes one configured plugin.
type Spec struct {
	// Name is how a check refers to this plugin.
	Name string

	// Command and Args start it.
	Command string
	Args    []string

	// Tool is the MCP tool to call for a fetch.
	Tool string

	// Env is the environment the plugin is given. Agentd does not pass its own
	// environment through: a plugin that inherits it inherits every credential
	// in it, including ones belonging to entirely unrelated checks.
	Env []string

	// SecretEnv maps an environment variable to the secret supplying it, so a
	// plugin can be given exactly the credentials it needs and no others.
	SecretEnv map[string]domain.SecretRef

	// Timeout bounds one call. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Host runs plugins on demand.
//
// Each fetch starts a process, talks to it, and stops it. Keeping a pool of
// long-lived plugin processes would be faster and would mean a plugin leaking
// memory over weeks becomes Agentd's problem; for a tool that fetches a page
// every few minutes, starting a process is not the expensive part.
type Host struct {
	mu    sync.RWMutex
	specs map[string]Spec
}

var _ ports.Source = (*Host)(nil)

// NewHost builds a Host from configured plugins.
func NewHost(specs ...Spec) *Host {
	h := &Host{specs: make(map[string]Spec, len(specs))}
	for _, s := range specs {
		h.specs[s.Name] = s
	}
	return h
}

// Register adds or replaces a plugin.
func (h *Host) Register(s Spec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.specs[s.Name] = s
}

// Names lists the configured plugins.
func (h *Host) Names() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.specs))
	for name := range h.specs {
		out = append(out, name)
	}
	return out
}

// Fetch runs a plugin and returns what it produced.
func (h *Host) Fetch(ctx context.Context, spec domain.SourceSpec, secrets domain.SecretBundle) (domain.RawResponse, error) {
	if spec.Kind != domain.SourcePlugin {
		return domain.RawResponse{}, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "wrong_adapter",
			Summary: fmt.Sprintf("this check is a %q source and cannot be fetched by a plugin", spec.Kind),
		}
	}

	h.mu.RLock()
	plugin, ok := h.specs[spec.Plugin]
	h.mu.RUnlock()
	if !ok {
		return domain.RawResponse{}, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "plugin_unknown",
			Summary: fmt.Sprintf("this check needs a plugin called %q, which is not configured", spec.Plugin),
		}
	}

	timeout := plugin.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session, err := start(ctx, plugin, secrets)
	if err != nil {
		return domain.RawResponse{}, err
	}
	defer session.stop()

	if err := session.initialise(ctx); err != nil {
		return domain.RawResponse{}, err
	}

	args := map[string]any{}
	for k, v := range spec.PluginArgs {
		args[k] = v
	}
	if spec.URL != "" {
		args["url"] = spec.URL
	}

	tool := plugin.Tool
	if tool == "" {
		tool = "fetch"
	}

	text, err := session.callTool(ctx, tool, args)
	if err != nil {
		return domain.RawResponse{}, err
	}

	return domain.RawResponse{
		ContentType: sniff(text),
		Body:        []byte(text),
		Status:      200,
		FetchedAt:   time.Now().UTC(),
	}, nil
}

// sniff guesses a content type from the payload, since a plugin reports none.
func sniff(s string) string {
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "{"), strings.HasPrefix(t, "["):
		return "application/json"
	case strings.HasPrefix(t, "<"):
		return "text/html"
	default:
		return "text/plain"
	}
}

// --- session ----------------------------------------------------------------

type session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *strings.Builder
	nextID int
}

// start launches a plugin process.
func start(ctx context.Context, spec Spec, secrets domain.SecretBundle) (*session, error) {
	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)

	// Exactly the environment the plugin is configured to need. Nothing is
	// inherited, so a plugin cannot read a credential belonging to a check it
	// has nothing to do with.
	env := append([]string(nil), spec.Env...)
	for name, ref := range spec.SecretEnv {
		secret, ok := secrets.Get(ref)
		if !ok {
			return nil, domain.Failure{
				Class:   domain.ClassAuth,
				Code:    "secret_missing",
				Summary: fmt.Sprintf("plugin %q needs a credential named %q that was not supplied", spec.Name, ref),
			}
		}
		env = append(env, name+"="+secret.Reveal())
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, pluginFailure(spec, "could not be started", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, pluginFailure(spec, "could not be started", err)
	}

	// Plugin stderr is captured rather than discarded: when a plugin fails,
	// what it printed on the way down is usually the only explanation anyone
	// is going to get.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, pluginFailure(spec, "could not be started", err)
	}

	return &session{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 1<<20),
		stderr: &stderr,
	}, nil
}

func (s *session) stop() {
	s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call sends one request and waits for its answer.
//
// Notifications sent by the plugin in the meantime are skipped rather than
// treated as answers: a plugin logging progress must not be mistaken for a
// plugin returning a result.
func (s *session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.nextID++
	id := s.nextID

	payload, err := json.Marshal(rpcRequest{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("plugins: encoding %s: %w", method, err)
	}
	if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
		return nil, s.crashed(err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, domain.Failure{
				Class:   domain.ClassTransient,
				Code:    "plugin_timeout",
				Summary: "the plugin did not answer in time",
				Detail:  s.stderrTail(),
			}
		}

		line, err := s.stdout.ReadBytes('\n')
		if err != nil {
			return nil, s.crashed(err)
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Not JSON-RPC at all. Plugins that print to stdout are common
			// enough that ignoring the line is kinder than failing.
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, domain.Failure{
				Class:   domain.ClassTransient,
				Code:    "plugin_error",
				Summary: "the plugin could not fetch this source",
				Detail:  resp.Error.Message,
			}
		}
		return resp.Result, nil
	}
}

// notify sends a request that expects no answer.
func (s *session) notify(method string, params any) error {
	payload, err := json.Marshal(rpcRequest{JSONRPC: jsonRPCVersion, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("plugins: encoding %s: %w", method, err)
	}
	if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
		return s.crashed(err)
	}
	return nil
}

// initialise performs the MCP handshake.
func (s *session) initialise(ctx context.Context) error {
	if _, err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentd", "version": "1"},
	}); err != nil {
		return err
	}
	return s.notify("notifications/initialized", map[string]any{})
}

// toolResult is the part of an MCP tool response Agentd reads.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// callTool invokes a tool and returns its text content.
func (s *session) callTool(ctx context.Context, tool string, args map[string]any) (string, error) {
	raw, err := s.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}

	var result toolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_unreadable",
			Summary: "the plugin's answer could not be read",
			Detail:  err.Error(),
		}
	}

	var text strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	if result.IsError {
		return "", domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_error",
			Summary: "the plugin could not fetch this source",
			Detail:  text.String(),
		}
	}
	if text.Len() == 0 {
		return "", domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_empty",
			Summary: "the plugin returned nothing to look at",
			Detail:  s.stderrTail(),
		}
	}
	return text.String(), nil
}

// crashed turns a broken pipe into a failure carrying whatever the plugin
// printed before it died.
func (s *session) crashed(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_stopped",
			Summary: "the plugin stopped before it answered",
			Detail:  s.stderrTail(),
		}
	}
	return domain.Failure{
		Class:   domain.ClassTransient,
		Code:    "plugin_io_failed",
		Summary: "Agentd lost contact with the plugin",
		Detail:  err.Error() + " " + s.stderrTail(),
	}
}

// stderrTail returns the last of what a plugin printed, bounded so that a
// plugin looping on an error message cannot fill a database row.
func (s *session) stderrTail() string {
	out := strings.TrimSpace(s.stderr.String())
	const max = 2000
	if len(out) > max {
		return "..." + out[len(out)-max:]
	}
	return out
}

func pluginFailure(spec Spec, what string, err error) error {
	return domain.Failure{
		Class:   domain.ClassFatal,
		Code:    "plugin_unstartable",
		Summary: fmt.Sprintf("the plugin %q %s", spec.Name, what),
		Detail:  err.Error(),
	}
}
