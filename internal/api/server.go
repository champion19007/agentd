package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

//go:embed ui/*
var uiFS embed.FS

// DefaultBindAddress is the loopback address and port Agentd binds to by default.
const DefaultBindAddress = "127.0.0.1:8080"

// IsLoopbackAddress reports whether addr resolves to a loopback host or IP.
func IsLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// ValidateBindAddress enforces the v1 security default: loopback binding only.
// Remote binding (e.g. 0.0.0.0 or external network interfaces) is strictly
// rejected unless allowRemote is explicitly set to true.
//
// Documented Security Model for v1:
// Agentd is a single-tenant, machine-local service. In v1, authentication and RBAC
// are intentionally out of scope. Binding to remote interfaces exposes the API
// unauthenticated and is forbidden by default.
func ValidateBindAddress(addr string, allowRemote bool) error {
	if allowRemote {
		return nil
	}
	if !IsLoopbackAddress(addr) {
		return fmt.Errorf("security: refusing to bind to non-loopback address %q (allowRemote is false); in v1 authentication is intentionally out of scope and the API must be loopback only", addr)
	}
	return nil
}

// Server manages the local HTTP API listener and server lifecycle.
type Server struct {
	addr        string
	allowRemote bool
	httpServer  *http.Server
	listener    net.Listener
}

// NewServer constructs an HTTP Server configured with security checks and timeouts.
func NewServer(addr string, ops Operations, allowRemote bool) (*Server, error) {
	if addr == "" {
		addr = DefaultBindAddress
	}
	if err := ValidateBindAddress(addr, allowRemote); err != nil {
		return nil, err
	}

	handler := NewHandler(ops)
	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return &Server{
		addr:        addr,
		allowRemote: allowRemote,
		httpServer:  httpSrv,
	}, nil
}

// Start begins listening and serving HTTP traffic.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = ln
	return s.httpServer.Serve(ln)
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// Addr returns the active listener address or configured address.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// Handler wraps Operations and returns an http.Handler serving the local REST API.
type Handler struct {
	ops       Operations
	mux       *http.ServeMux
	mcpServer *MCPServer
}

// NewHandler builds an http.Handler routing /v1/... requests to ops.
func NewHandler(ops Operations) http.Handler {
	h := &Handler{
		ops:       ops,
		mux:       http.NewServeMux(),
		mcpServer: NewMCPServer(ops),
	}
	h.registerRoutes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Guard against DNS rebinding and non-loopback hosts
	if r.Host != "" && !IsLoopbackAddress(r.Host) {
		http.Error(w, "forbidden: non-loopback Host header rejected", http.StatusForbidden)
		return
	}

	// Reject cross-site metadata from modern browsers (Sec-Fetch-Site: cross-site)
	if fetchSite := r.Header.Get("Sec-Fetch-Site"); fetchSite == "cross-site" {
		http.Error(w, "forbidden: cross-site requests rejected", http.StatusForbidden)
		return
	}

	// Guard against cross-origin browser requests (CSRF / drive-by attacks from external origins)
	if origin := r.Header.Get("Origin"); origin != "" {
		if origin == "null" {
			http.Error(w, "forbidden: opaque/null origin rejected", http.StatusForbidden)
			return
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || !IsLoopbackAddress(u.Host) {
			http.Error(w, "forbidden: cross-origin requests from external origins rejected", http.StatusForbidden)
			return
		}
	}

	// Cap request body size at 4 MiB to prevent unbounded memory consumption
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	}

	h.mux.ServeHTTP(w, r)
}

func (h *Handler) registerRoutes() {
	h.mux.HandleFunc("/v1/init", h.handleInit)
	h.mux.HandleFunc("/v1/status", h.handleStatus)
	h.mux.HandleFunc("/v1/checks", h.handleChecks)
	h.mux.HandleFunc("/v1/checks/", h.handleCheckItem)
	h.mux.HandleFunc("/v1/runs", h.handleRuns)
	h.mux.HandleFunc("/v1/runs/", h.handleRunItem)
	h.mux.HandleFunc("/v1/incidents", h.handleIncidents)
	h.mux.HandleFunc("/v1/incidents/", h.handleIncidentItem)
	h.mux.HandleFunc("/v1/gc", h.handleGC)
	h.mux.HandleFunc("/v1/backup", h.handleBackup)
	h.mux.HandleFunc("/v1/audit", h.handleAudit)
	h.mux.HandleFunc("/v1/mcp", h.handleMCP)
	h.mux.HandleFunc("/metrics", h.handleMetrics)
	h.mux.HandleFunc("/", h.handleUI)
}

func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	data, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, "ui dashboard not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed: MCP JSON-RPC requests must use POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	respBytes, err := h.mcpServer.HandleMessage(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(respBytes) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

func (h *Handler) handleInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req InitRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	resp, err := h.ops.Init(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp, err := h.ops.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleChecks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		checks, err := h.ops.ListChecks(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, checks)
	case http.MethodPost:
		var req AddCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		summary, err := h.ops.AddCheck(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, summary)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleCheckItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/checks/")
	parts := strings.Split(path, "/")
	id := domain.CheckID(parts[0])
	if id == "" {
		http.Error(w, "check id is required", http.StatusBadRequest)
		return
	}

	if len(parts) == 2 && parts[1] == "run" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		runSum, err := h.ops.RunCheck(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, runSum)
		return
	}
	if len(parts) > 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		detail, err := h.ops.GetCheck(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, detail)
	case http.MethodDelete:
		if err := h.ops.DeleteCheck(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "check disabled"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	traceID := r.URL.Query().Get("trace_id")
	if traceID != "" {
		detail, err := h.ops.GetRun(r.Context(), domain.RunID(traceID))
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, []RunSummary{detail.RunSummary})
		return
	}

	checkID := domain.CheckID(r.URL.Query().Get("check_id"))
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := h.ops.ListRuns(r.Context(), checkID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed: /metrics requires GET", http.StatusMethodNotAllowed)
		return
	}
	text := h.ops.ExportMetrics()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}

func (h *Handler) handleRunItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := domain.RunID(strings.TrimPrefix(r.URL.Path, "/v1/runs/"))
	detail, err := h.ops.GetRun(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *Handler) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	incidents, err := h.ops.ListIncidents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, incidents)
}

func (h *Handler) handleIncidentItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/incidents/")
	parts := strings.Split(path, "/")
	id := domain.IncidentID(parts[0])
	if id == "" {
		http.Error(w, "incident id is required", http.StatusBadRequest)
		return
	}

	if len(parts) == 2 {
		switch parts[1] {
		case "approve":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req ApproveRequest
			if r.Body != nil {
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
					writeError(w, http.StatusBadRequest, err)
					return
				}
			}
			res, err := h.ops.ApproveRepair(r.Context(), id, req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, res)
			return
		case "reject":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req RejectRequest
			if r.Body != nil {
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
					writeError(w, http.StatusBadRequest, err)
					return
				}
			}
			res, err := h.ops.RejectRepair(r.Context(), id, req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, res)
			return
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	if len(parts) > 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet {
		detail, err := h.ops.GetIncident(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, detail)
		return
	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (h *Handler) handleGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req GCRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	sweep, err := h.ops.GC(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sweep)
}

func (h *Handler) handleBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req BackupRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	res, err := h.ops.Backup(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 10000 {
		limit = 10000
	}
	events, err := h.ops.Audit(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) || (err != nil && strings.Contains(err.Error(), "request body too large")) {
		status = http.StatusRequestEntityTooLarge
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
