package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/champion19007/agentd/internal/core/domain"
)

// Client is an HTTP client implementing Operations against a remote or local agentd daemon.
type Client struct {
	baseURL string
	client  *http.Client
}

var _ Operations = (*Client)(nil)

// NewClient constructs a Client targeting baseURL (e.g. "http://localhost:8080").
func NewClient(baseURL string, client *http.Client) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
	}
}

func (c *Client) Init(ctx context.Context, req InitRequest) (InitResponse, error) {
	var resp InitResponse
	err := c.post(ctx, "/v1/init", req, &resp)
	return resp, err
}

func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	var resp StatusResponse
	err := c.get(ctx, "/v1/status", &resp)
	return resp, err
}

func (c *Client) AddCheck(ctx context.Context, req AddCheckRequest) (CheckSummary, error) {
	var resp CheckSummary
	err := c.post(ctx, "/v1/checks", req, &resp)
	return resp, err
}

func (c *Client) ListChecks(ctx context.Context) ([]CheckSummary, error) {
	var resp []CheckSummary
	err := c.get(ctx, "/v1/checks", &resp)
	return resp, err
}

func (c *Client) GetCheck(ctx context.Context, id domain.CheckID) (CheckDetail, error) {
	var resp CheckDetail
	err := c.get(ctx, "/v1/checks/"+url.PathEscape(string(id)), &resp)
	return resp, err
}

func (c *Client) RunCheck(ctx context.Context, id domain.CheckID) (RunSummary, error) {
	var resp RunSummary
	err := c.post(ctx, "/v1/checks/"+url.PathEscape(string(id))+"/run", nil, &resp)
	return resp, err
}

func (c *Client) DeleteCheck(ctx context.Context, id domain.CheckID) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/checks/"+url.PathEscape(string(id)), nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseError(resp)
	}
	return nil
}

func (c *Client) ListRuns(ctx context.Context, checkID domain.CheckID, limit int) ([]RunSummary, error) {
	var resp []RunSummary
	u := fmt.Sprintf("/v1/runs?check_id=%s&limit=%d", url.QueryEscape(string(checkID)), limit)
	err := c.get(ctx, u, &resp)
	return resp, err
}

func (c *Client) GetRun(ctx context.Context, id domain.RunID) (RunDetail, error) {
	var resp RunDetail
	err := c.get(ctx, "/v1/runs/"+url.PathEscape(string(id)), &resp)
	return resp, err
}

func (c *Client) ListIncidents(ctx context.Context) ([]IncidentSummary, error) {
	var resp []IncidentSummary
	err := c.get(ctx, "/v1/incidents", &resp)
	return resp, err
}

func (c *Client) GetIncident(ctx context.Context, id domain.IncidentID) (IncidentDetail, error) {
	var resp IncidentDetail
	err := c.get(ctx, "/v1/incidents/"+url.PathEscape(string(id)), &resp)
	return resp, err
}

func (c *Client) ApproveRepair(ctx context.Context, id domain.IncidentID, req ApproveRequest) (DecisionResult, error) {
	var resp DecisionResult
	err := c.post(ctx, "/v1/incidents/"+url.PathEscape(string(id))+"/approve", req, &resp)
	return resp, err
}

func (c *Client) RejectRepair(ctx context.Context, id domain.IncidentID, req RejectRequest) (DecisionResult, error) {
	var resp DecisionResult
	err := c.post(ctx, "/v1/incidents/"+url.PathEscape(string(id))+"/reject", req, &resp)
	return resp, err
}

func (c *Client) GC(ctx context.Context, req GCRequest) (domain.Sweep, error) {
	var resp domain.Sweep
	err := c.post(ctx, "/v1/gc", req, &resp)
	return resp, err
}

func (c *Client) Backup(ctx context.Context, req BackupRequest) (BackupResult, error) {
	var resp BackupResult
	err := c.post(ctx, "/v1/backup", req, &resp)
	return resp, err
}

func (c *Client) Audit(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	var resp []domain.AuditEvent
	err := c.get(ctx, "/v1/audit?limit="+strconv.Itoa(limit), &resp)
	return resp, err
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func parseError(resp *http.Response) error {
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil && errResp.Error != "" {
		return fmt.Errorf("api error (%d): %s", resp.StatusCode, errResp.Error)
	}
	return fmt.Errorf("api error: HTTP %d", resp.StatusCode)
}

func (c *Client) ExportMetrics() string {
	resp, err := c.client.Get(c.baseURL + "/metrics")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	bytes, _ := io.ReadAll(resp.Body)
	return string(bytes)
}
