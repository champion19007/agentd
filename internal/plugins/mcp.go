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
// Sandboxing & Security Invariants:
//   - Separate working directory (isolated sandbox per plugin).
//   - No inherited environment (minimal platform runtime only; never os.Environ()).
//   - No store access.
//   - Scoped secrets only (mapped explicitly via SecretEnv).
//   - Never log secrets (all error tails and stderr messages are scrubbed).
//   - Bounded lifecycle: lazy spawn, handshake capability negotiation, idle timeout,
//     and exponential backoff on crash.
package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// MCP Protocol constants.
const (
	ProtocolVersion = "2024-11-05"
	JSONRPCVersion  = "2.0"
)

// Default timing and backoff limits.
const (
	DefaultTimeout     = 60 * time.Second
	DefaultIdleTimeout = 5 * time.Minute
	DefaultBackoffBase = 500 * time.Millisecond
	DefaultMaxBackoff  = 30 * time.Second
	DefaultMaxRestarts = 3
)

// Spec describes the configuration, capabilities, and security boundary of a plugin.
type Spec struct {
	// Name is how a check refers to this plugin.
	Name string `json:"name"`

	// Command and Args specify the binary to execute.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`

	// Tool is the MCP tool to call for a fetch (default: "fetch").
	Tool string `json:"tool,omitempty"`

	// Dir specifies the isolated sandbox working directory. If empty, Host
	// creates a dedicated sandbox directory under the host's sandbox root.
	Dir string `json:"dir,omitempty"`

	// Env is the explicitly declared environment given to the plugin.
	// Agentd NEVER inherits the host daemon's environment.
	Env []string `json:"env,omitempty"`

	// SecretEnv maps an environment variable name to a SecretRef.
	// Only these scoped secrets are provided; no other credentials are leaked.
	SecretEnv map[string]domain.SecretRef `json:"secret_env,omitempty"`

	// Timeout bounds one tool call. Zero means DefaultTimeout.
	Timeout time.Duration `json:"timeout,omitempty"`

	// IdleTimeout is how long an idle process stays alive before being reaped.
	// Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"`

	// RequiredCapabilities are the capabilities the plugin MUST advertise during handshake.
	// If missing, handshake fails as a fatal configuration error.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`

	// MaxCrashRestarts is the number of restart attempts before extended backoff.
	MaxCrashRestarts int `json:"max_crash_restarts,omitempty"`

	// BackoffBase is the base duration for exponential backoff after a crash.
	BackoffBase time.Duration `json:"backoff_base,omitempty"`

	// MaxBackoff is the upper limit for crash backoff.
	MaxBackoff time.Duration `json:"max_backoff,omitempty"`
}

// Host manages the lifecycle, sandboxing, lazy spawning, and communication
// for MCP plugins running as stdio subprocesses.
type Host struct {
	mu          sync.RWMutex
	specs       map[string]Spec
	managed     map[string]*managedPlugin
	sandboxRoot string
	metrics     ports.Metrics
}

var _ ports.Source = (*Host)(nil)

// NewHost builds a Host from configured plugins.
func NewHost(specs ...Spec) *Host {
	h := &Host{
		specs:       make(map[string]Spec),
		managed:     make(map[string]*managedPlugin),
		sandboxRoot: filepath.Join(os.TempDir(), "agentd-sandboxes"),
	}
	for _, s := range specs {
		_ = h.Register(s)
	}
	return h
}

// WithMetrics attaches a metrics collector to the plugin host.
func (h *Host) WithMetrics(m ports.Metrics) *Host {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.metrics = m
	for _, mp := range h.managed {
		mp.metrics = m
	}
	return h
}

// SetSandboxRoot sets the root directory under which plugin sandbox folders are created.
func (h *Host) SetSandboxRoot(root string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sandboxRoot = root
}

// Register adds or replaces a plugin specification in the registry.
func (h *Host) Register(s Spec) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("plugin name is required")
	}
	if strings.TrimSpace(s.Command) == "" {
		return errors.New("plugin command is required")
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	if s.IdleTimeout <= 0 {
		s.IdleTimeout = DefaultIdleTimeout
	}
	if s.BackoffBase <= 0 {
		s.BackoffBase = DefaultBackoffBase
	}
	if s.MaxBackoff <= 0 {
		s.MaxBackoff = DefaultMaxBackoff
	}
	if s.MaxCrashRestarts <= 0 {
		s.MaxCrashRestarts = DefaultMaxRestarts
	}
	if s.Tool == "" {
		s.Tool = "fetch"
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// If already managed, stop any running process cleanly before replacing spec
	if old, ok := h.managed[s.Name]; ok {
		old.stop()
		delete(h.managed, s.Name)
	}

	h.specs[s.Name] = s
	return nil
}

// Get returns the Spec for a plugin if registered.
func (h *Host) Get(name string) (Spec, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.specs[name]
	return s, ok
}

// Names lists the configured plugins in the registry.
func (h *Host) Names() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.specs))
	for name := range h.specs {
		out = append(out, name)
	}
	return out
}

// Close stops all active plugin subprocesses and releases resources.
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.managed {
		m.stop()
	}
	h.managed = make(map[string]*managedPlugin)
	return nil
}

// VerifyCompatibility checks if a plugin starts and satisfies capability negotiation.
// Unsupported capabilities are reported as configuration errors.
func (h *Host) VerifyCompatibility(ctx context.Context, name string) error {
	mp, err := h.getOrCreateManaged(name)
	if err != nil {
		return err
	}
	return mp.verifyCompatibility(ctx)
}

func (h *Host) getOrCreateManaged(name string) (*managedPlugin, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	spec, ok := h.specs[name]
	if !ok {
		return nil, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "plugin_unknown",
			Summary: fmt.Sprintf("this check needs a plugin called %q, which is not configured", name),
		}
	}

	mp, ok := h.managed[name]
	if !ok {
		sandboxDir := spec.Dir
		if sandboxDir == "" {
			cleanName := filepath.Base(filepath.Clean(spec.Name))
			sandboxDir = filepath.Join(h.sandboxRoot, cleanName)
		}
		mp = &managedPlugin{
			spec:       spec,
			sandboxDir: sandboxDir,
			metrics:    h.metrics,
		}
		h.managed[name] = mp
	} else if mp.metrics == nil && h.metrics != nil {
		mp.metrics = h.metrics
	}
	return mp, nil
}

// Fetch executes a plugin tool call to retrieve content for a check.
func (h *Host) Fetch(ctx context.Context, spec domain.SourceSpec, secrets domain.SecretBundle) (domain.RawResponse, error) {
	if spec.Kind != domain.SourcePlugin {
		return domain.RawResponse{}, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "wrong_adapter",
			Summary: fmt.Sprintf("this check is a %q source and cannot be fetched by a plugin", spec.Kind),
		}
	}

	mp, err := h.getOrCreateManaged(spec.Plugin)
	if err != nil {
		return domain.RawResponse{}, err
	}

	args := map[string]any{}
	for k, v := range spec.PluginArgs {
		args[k] = v
	}
	if spec.URL != "" {
		args["url"] = spec.URL
	}

	tool := mp.spec.Tool
	if tool == "" {
		tool = "fetch"
	}

	text, err := mp.call(ctx, tool, args, secrets)
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

// sniff guesses a content type from the payload.
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

// --- managedPlugin ----------------------------------------------------------

type managedPlugin struct {
	mu               sync.Mutex
	spec             Spec
	sandboxDir       string
	activeSession    *session
	idleTimer        *time.Timer
	crashes          int
	lastCrash        time.Time
	nextAllowedStart time.Time
	capabilities     map[string]any
	metrics          ports.Metrics
}

func (mp *managedPlugin) stop() {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if mp.idleTimer != nil {
		mp.idleTimer.Stop()
		mp.idleTimer = nil
	}
	if mp.activeSession != nil {
		mp.activeSession.stop()
		mp.activeSession = nil
	}
}

func (mp *managedPlugin) onIdle() {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if mp.activeSession != nil {
		mp.activeSession.stop()
		mp.activeSession = nil
	}
}

func (mp *managedPlugin) recordCrash(err error) {
	var dFail domain.Failure
	if errors.As(err, &dFail) {
		if dFail.Class == domain.ClassAuth || dFail.Class == domain.ClassFatal {
			return
		}
	}

	mp.crashes++
	mp.lastCrash = time.Now()

	// Exponential backoff: base * 2^(crashes-1), bounded by MaxBackoff
	shift := mp.crashes - 1
	if shift > 6 {
		shift = 6
	}
	backoff := mp.spec.BackoffBase * time.Duration(1<<shift)
	if backoff > mp.spec.MaxBackoff {
		backoff = mp.spec.MaxBackoff
	}
	mp.nextAllowedStart = time.Now().Add(backoff)
}

func (mp *managedPlugin) verifyCompatibility(ctx context.Context) error {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	// If already running and negotiated, check capabilities
	if len(mp.capabilities) > 0 {
		return mp.checkCapabilities()
	}

	// Probe spawn
	sess, err := mp.spawnAndHandshake(ctx, domain.SecretBundle{})
	if err != nil {
		return err
	}
	defer sess.stop()
	return mp.checkCapabilities()
}

func (mp *managedPlugin) checkCapabilities() error {
	for _, reqCap := range mp.spec.RequiredCapabilities {
		if _, ok := mp.capabilities[reqCap]; !ok {
			return domain.Failure{
				Class:   domain.ClassFatal,
				Code:    "capability_unsupported",
				Summary: fmt.Sprintf("plugin %q lacks required capability %q", mp.spec.Name, reqCap),
				Detail:  "unsupported capabilities are configuration errors at load",
			}
		}
	}
	return nil
}

func (mp *managedPlugin) call(ctx context.Context, tool string, args map[string]any, secrets domain.SecretBundle) (string, error) {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	// Stop idle timer while a call is in progress
	if mp.idleTimer != nil {
		mp.idleTimer.Stop()
		mp.idleTimer = nil
	}

	// Ensure idle timer is re-armed if active session remains alive when call returns
	defer func() {
		if mp.activeSession != nil && !mp.activeSession.isDead() {
			idleTimeout := mp.spec.IdleTimeout
			if idleTimeout <= 0 {
				idleTimeout = DefaultIdleTimeout
			}
			if mp.idleTimer != nil {
				mp.idleTimer.Stop()
			}
			mp.idleTimer = time.AfterFunc(idleTimeout, func() {
				mp.onIdle()
			})
		}
	}()

	// Check crash backoff
	now := time.Now()
	if mp.crashes > 0 && now.Before(mp.nextAllowedStart) {
		if mp.metrics != nil {
			mp.metrics.RecordPluginCall(mp.spec.Name, "circuit_broken")
		}
		remaining := mp.nextAllowedStart.Sub(now)
		return "", domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_backoff",
			Summary: fmt.Sprintf("plugin %q crashed recently; backing off (%v remaining)", mp.spec.Name, remaining.Round(time.Millisecond)),
			Detail:  "a broken plugin degrades only the check depending on it",
		}
	}

	// Lazy spawn: start subprocess only when needed
	if mp.activeSession == nil || mp.activeSession.isDead() {
		sess, err := mp.spawnAndHandshake(ctx, secrets)
		if err != nil {
			if mp.metrics != nil {
				mp.metrics.RecordPluginCall(mp.spec.Name, "error")
			}
			mp.recordCrash(err)
			return "", err
		}
		mp.activeSession = sess
	}

	// Enforce per-call timeout
	timeout := mp.spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resText, err := mp.activeSession.callTool(callCtx, tool, args)
	if err != nil {
		// Process termination on timeout
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) || (ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
			if mp.metrics != nil {
				mp.metrics.RecordPluginCall(mp.spec.Name, "timeout")
			}
			mp.activeSession.stop()
			mp.activeSession = nil
			mp.recordCrash(err)
			return "", domain.Failure{
				Class:   domain.ClassTransient,
				Code:    "plugin_timeout",
				Summary: fmt.Sprintf("plugin %q did not answer within %v", mp.spec.Name, timeout),
				Detail:  "the plugin process was terminated on timeout",
			}
		}

		if mp.metrics != nil {
			mp.metrics.RecordPluginCall(mp.spec.Name, "error")
		}

		// Process crash or I/O failure
		if mp.activeSession.isDead() {
			tail := mp.activeSession.stderrTail()
			scrubbedErr := errors.New(mp.activeSession.scrub(err.Error()))
			mp.activeSession.stop()
			mp.activeSession = nil
			mp.recordCrash(err)
			return "", classifyPluginError(scrubbedErr, tail)
		}

		scrubbedErr := errors.New(mp.activeSession.scrub(err.Error()))
		return "", classifyPluginError(scrubbedErr, mp.activeSession.stderrTail())
	}

	// Call succeeded: reset crash count
	mp.crashes = 0
	if mp.metrics != nil {
		mp.metrics.RecordPluginCall(mp.spec.Name, "ok")
	}
	return resText, nil
}

func (mp *managedPlugin) spawnAndHandshake(ctx context.Context, secrets domain.SecretBundle) (*session, error) {
	// 1. Separate working directory
	if err := os.MkdirAll(mp.sandboxDir, 0700); err != nil {
		return nil, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "sandbox_init_failed",
			Summary: fmt.Sprintf("failed to create sandbox directory for plugin %q: %v", mp.spec.Name, err),
		}
	}

	// 2. No inherited environment & Scoped secrets only
	env, secretValues, err := buildPluginEnv(mp.spec, secrets)
	if err != nil {
		return nil, err
	}

	sess, err := startSession(ctx, mp.spec, mp.sandboxDir, env, secretValues)
	if err != nil {
		return nil, err
	}

	// 3. MCP Handshake
	rawInit, err := sess.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentd", "version": "1.0.0"},
	})
	if err != nil {
		sess.stop()
		return nil, domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_handshake_failed",
			Summary: fmt.Sprintf("plugin %q failed initialize handshake", mp.spec.Name),
			Detail:  sess.scrub(err.Error() + " " + sess.stderrTail()),
		}
	}

	// 4. Capability negotiation
	var initResp struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	_ = json.Unmarshal(rawInit, &initResp)
	mp.capabilities = initResp.Capabilities

	if err := mp.checkCapabilities(); err != nil {
		sess.stop()
		return nil, err
	}

	// Send notifications/initialized
	_ = sess.notify("notifications/initialized", map[string]any{})

	return sess, nil
}

// buildPluginEnv ensures no parent environment is inherited, and only scoped secrets
// and minimal system variables are provided.
func buildPluginEnv(spec Spec, secrets domain.SecretBundle) ([]string, []string, error) {
	env := minimalEnv()
	env = append(env, spec.Env...)

	var secretValues []string
	for envVar, ref := range spec.SecretEnv {
		sec, ok := secrets.Get(ref)
		if !ok {
			return nil, nil, domain.Failure{
				Class:   domain.ClassAuth,
				Code:    "secret_missing",
				Summary: fmt.Sprintf("plugin %q needs a credential named %q that was not supplied", spec.Name, ref),
			}
		}
		val := sec.Reveal()
		env = append(env, envVar+"="+val)
		if len(val) > 0 {
			secretValues = append(secretValues, val)
		}
	}
	return env, secretValues, nil
}

func minimalEnv() []string {
	var env []string
	if runtime.GOOS == "windows" {
		keys := []string{"SYSTEMROOT", "SystemRoot", "COMSPEC", "ComSpec", "PATH", "Path", "TMP", "TEMP"}
		for _, k := range keys {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	} else {
		keys := []string{"PATH", "TMPDIR", "USER", "HOME"}
		for _, k := range keys {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	return env
}

// --- session ----------------------------------------------------------------

type safeStderr struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeStderr) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > 64*1024 {
		s.buf.Reset()
	}
	return s.buf.Write(p)
}

func (s *safeStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type session struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       *bufio.Reader
	stderr       *safeStderr
	secretValues []string
	nextID       int
	dead         bool
	mu           sync.Mutex
}

func startSession(ctx context.Context, spec Spec, dir string, env []string, secretValues []string) (*session, error) {
	// Subprocess lifetime is independent from the caller request context so cached
	// sessions survive between checks; per-call execution deadlines are bounded on callTool.
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = dir
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "plugin_unstartable",
			Summary: fmt.Sprintf("plugin %q stdin pipe failed: %v", spec.Name, err),
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "plugin_unstartable",
			Summary: fmt.Sprintf("plugin %q stdout pipe failed: %v", spec.Name, err),
		}
	}

	stderr := &safeStderr{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "plugin_unstartable",
			Summary: fmt.Sprintf("plugin %q could not be started: %v", spec.Name, err),
		}
	}

	return &session{
		cmd:          cmd,
		stdin:        stdin,
		stdout:       bufio.NewReaderSize(stdout, 1<<20),
		stderr:       stderr,
		secretValues: secretValues,
	}, nil
}

func (s *session) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

func (s *session) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	s.dead = true
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = killProcessTree(s.cmd.Process)
	}
	if s.cmd != nil {
		done := make(chan struct{})
		go func() {
			_ = s.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

// killProcessTree terminates a process and any child processes it may have spawned.
// On Windows, child processes (e.g. spawned by node.exe or python.exe) are not
// automatically cleaned up by TerminateProcess / Process.Kill without Job Objects.
// We immediately invoke Process.Kill() to unblock execution, and on Windows asynchronously
// sweep the process tree with taskkill /T /F /PID as the safest available cleanup mechanism.
func killProcessTree(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	err := proc.Kill()
	if runtime.GOOS == "windows" {
		pid := proc.Pid
		go func() {
			killCmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
			_ = killCmd.Run()
		}()
	}
	return err
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

func (s *session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()

	payload, err := json.Marshal(rpcRequest{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", method, err)
	}

	if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
		s.markDead()
		return nil, s.crashed(err)
	}

	// Active watcher: unblock ReadBytes and kill process immediately on timeout/cancellation
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			s.stop()
		case <-stopWatch:
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line, err := s.stdout.ReadBytes('\n')
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			s.markDead()
			return nil, s.crashed(err)
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Ignore non-JSON lines printed to stdout
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, errors.New(s.scrub(resp.Error.Message))
		}
		return resp.Result, nil
	}
}

func (s *session) notify(method string, params any) error {
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("encoding %s: %w", method, err)
	}
	if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
		s.markDead()
		return s.crashed(err)
	}
	return nil
}

func (s *session) markDead() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = true
}

func (s *session) callTool(ctx context.Context, tool string, args map[string]any) (string, error) {
	raw, err := s.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_unreadable",
			Summary: "the plugin's answer could not be read",
			Detail:  err.Error(),
		}
	}

	var sb strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	if result.IsError {
		return "", errors.New(s.scrub(sb.String()))
	}
	text := sb.String()
	if len(strings.TrimSpace(text)) == 0 {
		return "", domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_empty",
			Summary: "the plugin returned an empty response",
			Detail:  s.stderrTail(),
		}
	}
	return text, nil
}

func (s *session) crashed(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_stopped",
			Summary: "the plugin stopped before answering",
			Detail:  s.stderrTail(),
		}
	}
	return domain.Failure{
		Class:   domain.ClassTransient,
		Code:    "plugin_io_failed",
		Summary: "lost contact with the plugin",
		Detail:  s.scrub(err.Error()) + " " + s.stderrTail(),
	}
}

func (s *session) stderrTail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := strings.TrimSpace(s.stderr.String())
	const max = 2000
	if len(out) > max {
		out = "..." + out[len(out)-max:]
	}
	return s.scrub(out)
}

func (s *session) scrub(text string) string {
	for _, sec := range s.secretValues {
		if len(sec) > 0 {
			text = strings.ReplaceAll(text, sec, "[REDACTED]")
		}
	}
	return text
}

// classifyPluginError maps any plugin error or stderr message into domain.FailureClass.
func classifyPluginError(err error, stderr string) domain.Failure {
	if err == nil {
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_error",
			Summary: "unknown plugin failure",
			Detail:  stderr,
		}
	}

	var dFail domain.Failure
	if errors.As(err, &dFail) {
		return dFail
	}

	msg := strings.ToLower(err.Error() + " " + stderr)

	// Rate limiting: 429, rate limit, too many requests
	if strings.Contains(msg, "429") || strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "rate_limited") {
		return domain.Failure{
			Class:   domain.ClassRateLimited,
			Code:    "plugin_rate_limited",
			Summary: "the plugin or upstream source reported rate limiting",
			Detail:  err.Error(),
		}
	}

	// Auth: 401, 403, unauthorized, forbidden, secret_missing, authenticat
	if strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "access denied") || strings.Contains(msg, "authenticat") {
		return domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "plugin_auth_failed",
			Summary: fmt.Sprintf("the plugin reported an authentication failure: %s", err.Error()),
			Detail:  err.Error() + " " + stderr,
		}
	}

	// Fatal: unsupported capability, executable not found
	if strings.Contains(msg, "capability") || strings.Contains(msg, "executable file not found") {
		return domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "plugin_fatal",
			Summary: fmt.Sprintf("the plugin configuration or binary is invalid: %s", err.Error()),
			Detail:  err.Error() + " " + stderr,
		}
	}

	// Timeout
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "timeout") {
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "plugin_timeout",
			Summary: "the plugin did not answer in time",
			Detail:  stderr,
		}
	}

	// Transient default
	return domain.Failure{
		Class:   domain.ClassTransient,
		Code:    "plugin_transient",
		Summary: fmt.Sprintf("the plugin failed: %s", err.Error()),
		Detail:  err.Error() + " " + stderr,
	}
}
