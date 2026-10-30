//go:build !(js && wasm)

package shell

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/ingest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// RemoteQueryResult is what the shell gets back from a Query RPC.
type RemoteQueryResult struct {
	Rows    []*pb.ResultRow
	Text    string
	Notices []RemoteNotice
	// FinalDirStack is the post-run directory stack (index 0 == cwd) the
	// server reports via the SSE "cwd" event — the ferry-back half of the
	// dirstack courier. Nil when the stream carried no cwd event.
	FinalDirStack []string
}

// RemoteNotice mirrors the proto Notice message decoded from SSE.
// The shell renders these as a prepended block before the actual
// output (see RenderNotices).
type RemoteNotice struct {
	Key   string
	Kind  string
	Title string
	Body  string
}

// SourceInfo describes a configured log source as returned by
// GET /api/sources. Tags match the api server's JSON response so
// encoding/json decodes cleanly into it.
type SourceInfo struct {
	Namespace  string `json:"namespace"`
	Service    string `json:"service"`
	Status     string `json:"status"`
	SourceType string `json:"source_type"`
	LogGroup   string `json:"log_group"`
	Region     string `json:"region"`
	Enabled    bool   `json:"enabled"`
	// Agents is who has been reading a weftd source lately; nil if no agent
	// has reported (or the source is not a weftd one).
	Agents *AgentFleet `json:"agents,omitempty"`
	// Resample is set while a configured source is being re-sampled, or once
	// the fresh sample is waiting for review.
	Resample *ResampleInfo `json:"resample,omitempty"`
}

// ResampleInfo is a configured source's resample progress.
type ResampleInfo struct {
	Collected int  `json:"collected"`
	Target    int  `json:"target"`
	Ready     bool `json:"ready"`
}

// AgentFleet summarizes the agents reading a source.
type AgentFleet struct {
	Count  int    `json:"count"`
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
}

// TokenInfo describes an agent token as returned by GET /api/tokens.
// The raw token is only present in the POST /api/tokens response; it
// is never re-served by the API. Callers identify a token by its opaque
// public ID (tok_...) for revoke.
type TokenInfo struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Namespace string `json:"namespace"`
	Env       string `json:"env"`
	Status    string `json:"status"` // "active" | "revoked"
	CreatedAt string `json:"created_at"`
}

// HTTPCoordinator talks to the weft API over HTTP instead of gRPC.
// This works through the ALB / public internet.
//
// Two clients by design:
//
//   - client: one-shot RPCs (Query, ListApps, token/namespace CRUD, source
//     management). http.Client.Timeout applies to the whole request
//     including the body read, which is exactly right for a short
//     request-reply exchange — it caps how long we'll wait on an
//     unresponsive server.
//   - streamClient: long-lived SSE streams (Follow). Here the body read
//     is the entire point, so a wall-clock Timeout is wrong: it would
//     guarantee the connection dies N seconds in regardless of whether
//     the user or server wanted it to. Cancellation flows through the
//     caller-supplied ctx instead.
//
// Keep the split explicit so a new endpoint has to pick its intent
// rather than inheriting the wrong timeout by accident.
type HTTPCoordinator struct {
	baseURL      string // e.g., "https://api.w3ft.net"
	token        string // JWT for Authorization header
	org          string // X-Active-Org for the user's currently-selected org
	clientID     string // X-Weft-Client; set by cmd/weft via SetClientID
	client       remoteClient
	streamClient remoteClient
}

func NewHTTPCoordinator(baseURL, token, org string) *HTTPCoordinator {
	return &HTTPCoordinator{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		token:        token,
		org:          org,
		client:       remoteClient{&http.Client{Timeout: 10 * time.Second}},
		streamClient: remoteClient{&http.Client{}},
	}
}

// SetClientID sets the X-Weft-Client header value. cmd/weft populates this
// from the build-time ClientID() so the API server can read the binary
// version on every request. Leaving it empty means the request omits the
// header (used by unit tests that don't care about the gate).
func (h *HTTPCoordinator) SetClientID(id string) {
	h.clientID = id
}

// setAuthHeaders applies Authorization + X-Active-Org + X-Weft-Client to
// every outgoing request. Centralized so a new endpoint can't forget any
// of them.
func (h *HTTPCoordinator) setAuthHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+h.token)
	if h.org != "" {
		req.Header.Set("X-Active-Org", h.org)
	}
	if h.clientID != "" {
		req.Header.Set("X-Weft-Client", h.clientID)
	}
}

func (h *HTTPCoordinator) Close() error { return nil }

// httpError turns a non-2xx response into a CLI-friendly error. 401 always
// means the same thing to a human at the terminal, regardless of which
// endpoint said it, so every caller gets the same "run weft login" hint
// instead of a raw status code — weft sh's first call (ListApps, via
// connectRemote in cmd/weft/main.go) used to leak "HTTP 401" straight to
// the terminal on an expired/invalid token.
func httpError(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("unauthorized — run `weft login` or check WEFT_TOKEN")
	}
	msg, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

// errQueryTimedOut is what a user sees when a query's connection times out
// on the client's side. It says what to do about it, without naming anything
// of ours.
var errQueryTimedOut = errors.New("query timed out — narrow the time range, filter earlier in the pipeline, or run `explain` on it to see where the time goes")

// Query runs a one-shot pipeline against the given range spec. rangeSpec is
// the un-resolved string the user typed ("1d..", "30m..10m",
// "2026-04-01..2026-04-12"); the API server resolves it against its own clock
// per request. See [[feedback_no_legacy_support]] for why we don't pass
// pre-resolved nanos.
func (h *HTTPCoordinator) Query(pipeline, rangeSpec string, dirStack []string) (*RemoteQueryResult, error) {
	body, _ := json.Marshal(map[string]any{
		"pipeline": pipeline,
		"follow":   false,
		"range":    rangeSpec,
		"dirstack": dirStack,
	})

	req, err := http.NewRequest("POST", h.baseURL+"/api/shell/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	// A query has no deadline of the client's: how long it may run is the
	// server's to say (its query deadline, which answers with its own message),
	// not the short deadline the other calls have.
	resp, err := h.streamClient.Do(req)
	if err != nil {
		if isTimeout(err) {
			return nil, errQueryTimedOut
		}
		return nil, fmt.Errorf("query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUpgradeRequired {
		return nil, formatUpgradeRequired(resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}

	return parseSSEResponse(resp.Body)
}

// formatUpgradeRequired turns the server's 426 JSON body into a
// human-friendly error. The server response shape is
// {"error":"client too old","running":"…","required":"…","upgrade":"…"} —
// surface the required version + the upgrade hint so the user knows
// exactly what to do without dumping a stack trace.
func formatUpgradeRequired(body io.Reader) error {
	var payload struct {
		Error    string `json:"error"`
		Running  string `json:"running"`
		Required string `json:"required"`
		Upgrade  string `json:"upgrade"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return fmt.Errorf("this weft CLI is too old for the server — run `weft update`")
	}
	hint := payload.Upgrade
	if hint == "" {
		hint = "run `weft update`"
	}
	return fmt.Errorf("this weft CLI (%s) is too old — server requires %s. %s",
		payload.Running, payload.Required, hint)
}

func (h *HTTPCoordinator) Follow(ctx context.Context, pipeline, rangeSpec string, onRow func(*pb.ResultRow)) error {
	body, _ := json.Marshal(map[string]any{
		"pipeline": pipeline,
		"follow":   true,
		"range":    rangeSpec,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", h.baseURL+"/api/shell/exec", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	// streamClient has no wall-clock Timeout — the request lives for the
	// duration of the caller's ctx.
	resp, err := h.streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUpgradeRequired {
		return formatUpgradeRequired(resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}

	// bufio.NewScanner's default MaxScanTokenSize is 64KB. Real log lines
	// routinely run longer (stack traces, JSON blobs with large payloads);
	// lifting the ceiling to 10MB costs nothing at steady state and
	// avoids silent ErrTooLong terminations mid-stream.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}

		switch event["type"] {
		case "line":
			if rowData, ok := event["data"].(map[string]any); ok {
				onRow(mapToResultRow(rowData))
			}
		case "error":
			if msg, ok := event["message"].(string); ok {
				return fmt.Errorf("%s", msg)
			}
		}
	}
	return scanner.Err()
}

func (h *HTTPCoordinator) ListApps() ([]string, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/apps", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}

	var apps []string
	json.NewDecoder(resp.Body).Decode(&apps)
	return apps, nil
}

// Client returns nil — HTTP mode doesn't have a gRPC client.
func (h *HTTPCoordinator) Client() pb.WeftCoordinatorClient { return nil }

// NamespaceInfo holds namespace metadata.
type NamespaceInfo struct {
	Name string `json:"name"`
	ID   uint   `json:"id"`
}

func (h *HTTPCoordinator) ListNamespaces() ([]NamespaceInfo, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/namespaces", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpError(resp)
	}
	var result struct {
		Namespaces []NamespaceInfo `json:"namespaces"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Namespaces, nil
}

func (h *HTTPCoordinator) CreateNamespace(name string) (*NamespaceInfo, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequest("POST", h.baseURL+"/api/namespaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpError(resp)
	}
	var ns NamespaceInfo
	json.NewDecoder(resp.Body).Decode(&ns)
	return &ns, nil
}

// CreateToken mints an agent token scoped to one namespace (and, optionally, env).
func (h *HTTPCoordinator) CreateToken(customer, label, namespace, env string) (token string, err error) {
	body, _ := json.Marshal(map[string]string{"label": label, "namespace": namespace, "env": env})
	req, _ := http.NewRequest("POST", h.baseURL+"/api/tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", httpError(resp)
	}
	var result struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Token, nil
}

func (h *HTTPCoordinator) ListTokens(customer string) ([]TokenInfo, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/tokens", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpError(resp)
	}
	var tokens []TokenInfo
	json.NewDecoder(resp.Body).Decode(&tokens)
	return tokens, nil
}

func (h *HTTPCoordinator) RevokeToken(id string) error {
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/api/tokens/%s", h.baseURL, url.PathEscape(id)), nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// IntegrationsStatus mirrors GET /api/integrations — the connection
// state for each notification backend. PagerDuty keys are masked by
// the server (first/last four chars), so this struct only ever holds
// display-safe values.
type IntegrationsStatus struct {
	Slack struct {
		Connected bool   `json:"connected"`
		TeamName  string `json:"team_name"`
	} `json:"slack"`
	PagerDuty struct {
		Connected    bool                   `json:"connected"`
		Keys         map[string]string      `json:"keys"` // name → masked key
		Integrations []PagerDutyIntegration `json:"integrations"`
	} `json:"pagerduty"`
	// WebhookSecrets are the names only; the server never returns a value.
	WebhookSecrets []string `json:"webhook_secrets"`
}

// PagerDutyIntegration is one named PagerDuty target as returned in
// the integrations status. LastUsedAt is a nullable RFC3339 string.
type PagerDutyIntegration struct {
	Name       string  `json:"name"`
	LastUsedAt *string `json:"last_used_at"`
	LastError  string  `json:"last_error"`
}

// ListIntegrations fetches the current Slack + PagerDuty status.
func (h *HTTPCoordinator) ListIntegrations() (*IntegrationsStatus, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/integrations", nil)
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpError(resp)
	}
	var status IntegrationsStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}

// SlackConnectURL starts the Slack OAuth flow and returns the
// authorization URL the user must open to grant the workspace. The
// handshake completes server-side via the OAuth callback; the CLI
// confirms by polling ListIntegrations.
func (h *HTTPCoordinator) SlackConnectURL() (string, error) {
	req, _ := http.NewRequest("POST", h.baseURL+"/api/integrations/slack/connect", nil)
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", httpError(resp)
	}
	var result struct {
		URL string `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.URL, nil
}

// SlackDisconnect removes the Slack integration.
func (h *HTTPCoordinator) SlackDisconnect() error {
	req, _ := http.NewRequest("DELETE", h.baseURL+"/api/integrations/slack", nil)
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// SavePagerDuty creates or updates a named PagerDuty integration key.
func (h *HTTPCoordinator) SavePagerDuty(name, key string) error {
	body, _ := json.Marshal(map[string]string{"name": name, "key": key})
	req, _ := http.NewRequest("PUT", h.baseURL+"/api/integrations/pagerduty", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// DeletePagerDuty removes a single PagerDuty integration by name.
func (h *HTTPCoordinator) DeletePagerDuty(name string) error {
	req, _ := http.NewRequest("DELETE", h.baseURL+"/api/integrations/pagerduty/"+url.PathEscape(name), nil)
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// SaveWebhookSecret creates or replaces a named webhook secret.
func (h *HTTPCoordinator) SaveWebhookSecret(name, value string) error {
	body, _ := json.Marshal(map[string]string{"name": name, "value": value})
	req, _ := http.NewRequest("PUT", h.baseURL+"/api/integrations/webhook-secrets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// DeleteWebhookSecret removes a webhook secret by name.
func (h *HTTPCoordinator) DeleteWebhookSecret(name string) error {
	req, _ := http.NewRequest("DELETE", h.baseURL+"/api/integrations/webhook-secrets/"+url.PathEscape(name), nil)
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// stringOf coerces an `any` from a JSON-decoded map into a string,
// returning "" for any non-string or nil value. Used by the SSE
// notice parser where the protocol guarantees string fields but the
// generic decoder hands back interface{}.
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// RenderNotice formats a single Notice as a multi-line block ready
// to be printed before a command's output. Title is highlighted via
// ANSI color keyed on Kind: success green, warning yellow, error red.
// Body is left as-is (may be multi-line). The block ends in a
// horizontal rule so the actual output is visually separated.
func RenderNotice(n RemoteNotice) string {
	var color string
	switch n.Kind {
	case "success":
		color = "\033[38;5;2m" // green
	case "error":
		color = "\033[38;5;1m" // red
	default: // warning + unknown
		color = "\033[38;5;3m" // yellow
	}
	const reset = "\033[0m"
	const rule = "─────────────────────────────────────────────────────\n"
	return rule + color + n.Title + reset + "\n" + n.Body + "\n" + rule + "\n"
}

// ── SSE parsing ──────────────────────────────────────────────

func parseSSEResponse(r io.Reader) (*RemoteQueryResult, error) {
	result := &RemoteQueryResult{}
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}

		switch event["type"] {
		case "line":
			if rowData, ok := event["data"].(map[string]any); ok {
				result.Rows = append(result.Rows, mapToResultRow(rowData))
			}
		case "stderr":
			// Render stderr in red ANSI so the shell displays it appropriately.
			if text, ok := event["text"].(string); ok {
				result.Text += "\033[38;5;1m" + text + "\033[0m"
			}
		case "notice":
			result.Notices = append(result.Notices, RemoteNotice{
				Key:   stringOf(event["key"]),
				Kind:  stringOf(event["kind"]),
				Title: stringOf(event["title"]),
				Body:  stringOf(event["body"]),
			})
		case "cwd":
			// Dirstack courier (ferry back): the server reports the
			// post-run stack so the client can cache the cwd a shipped
			// cd/pushd established. index 0 is cwd.
			if raw, ok := event["stack"].([]any); ok {
				stack := make([]string, 0, len(raw))
				for _, v := range raw {
					stack = append(stack, stringOf(v))
				}
				result.FinalDirStack = stack
			}
		case "error":
			if msg, ok := event["message"].(string); ok {
				return nil, fmt.Errorf("%s", msg)
			}
		case "done":
			return result, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// PendingSource represents a source awaiting schema configuration.
type PendingSource struct {
	Service    string          `json:"service"`
	Namespace  string          `json:"namespace"`
	Instance   string          `json:"instance"`
	Deployment string          `json:"deployment"`
	Sample     json.RawMessage `json:"sample"`
	Detection  json.RawMessage `json:"detection"`
	CreatedAt  string          `json:"created_at"`
	// Status is "sampling" or "pending"; SampleCollected/SampleTarget are
	// the fill progress and SampleUpdatedAt (RFC3339) when the newest
	// lines arrived.
	Status          string `json:"status"`
	SampleCollected int    `json:"sample_collected"`
	SampleTarget    int    `json:"sample_target"`
	SampleUpdatedAt string `json:"sample_updated_at"`
}

// ListPendingSources returns all pending sources for the authenticated customer.
func (h *HTTPCoordinator) ListPendingSources(namespace string) ([]PendingSource, error) {
	url := h.baseURL + "/api/sources/pending"
	if namespace != "" {
		url += "?namespace=" + namespace
	}
	req, _ := http.NewRequest("GET", url, nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}

	var sources []PendingSource
	if err := json.NewDecoder(resp.Body).Decode(&sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// ConfigureSource confirms the schema for a pending source.
func (h *HTTPCoordinator) ConfigureSource(namespace, service string, config map[string]any) error {
	body, _ := json.Marshal(config)
	url := fmt.Sprintf("%s/api/sources/%s/%s/configure", h.baseURL, namespace, service)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

func mapToResultRow(m map[string]any) *pb.ResultRow {
	row := &pb.ResultRow{}
	if v, ok := m["raw"].(string); ok {
		row.Raw = v
	}
	return row
}

// ── Sources (CloudWatch push + weftd) ───────────────────────────

// AddKinesisSourceRequest mirrors the JSON body accepted by
// POST /api/sources/kinesis. No cross-account role (nothing runs in
// weft's account for this path — see
// docs/plans/cloudwatch-kinesis-subscription.md) and no schema fields
// (the source is created StatusPending; `weft source configure`
// finishes it once real events have started flowing through the
// customer's Firehose stream and a sample has accumulated).
type AddKinesisSourceRequest struct {
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	LogGroup  string `json:"log_group"`
	Region    string `json:"region"`
}

// AddKinesisSource registers the AWS-log-group→service mapping for a
// Firehose push source.
func (h *HTTPCoordinator) AddKinesisSource(req AddKinesisSourceRequest) error {
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", h.baseURL+"/api/sources/kinesis", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(r)

	resp, err := h.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// ListSources returns every configured source for the caller's customer.
func (h *HTTPCoordinator) ListSources() ([]SourceInfo, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/sources", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpError(resp)
	}
	var sources []SourceInfo
	if err := json.NewDecoder(resp.Body).Decode(&sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// RemoveSource deletes the source at (namespace, app). app is the
// SUB-app name (not "namespace/app"); the api rebuilds the full key.
func (h *HTTPCoordinator) RemoveSource(namespace, app string) error {
	url := fmt.Sprintf("%s/api/sources/%s/%s", h.baseURL, namespace, app)
	req, _ := http.NewRequest("DELETE", url, nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// NamespaceNotEmptyError is the server's refusal to remove a namespace that
// still holds sources or live tokens; retry with force to remove it anyway.
type NamespaceNotEmptyError struct {
	Namespace string
	Sources   []string
	Tokens    int
}

func (e *NamespaceNotEmptyError) Error() string {
	return fmt.Sprintf("namespace %q is not empty (%d sources, %d live tokens)", e.Namespace, len(e.Sources), e.Tokens)
}

// RemoveNamespace deletes a namespace. Without force the server refuses a
// namespace that still has sources or live tokens, and the returned error is a
// *NamespaceNotEmptyError describing them.
func (h *HTTPCoordinator) RemoveNamespace(name string, force bool) error {
	url := fmt.Sprintf("%s/api/namespaces/%s", h.baseURL, name)
	if force {
		url += "?force=true"
	}
	req, _ := http.NewRequest("DELETE", url, nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		var body struct {
			Sources []string `json:"sources"`
			Tokens  int      `json:"tokens"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil {
			return &NamespaceNotEmptyError{Namespace: name, Sources: body.Sources, Tokens: body.Tokens}
		}
	}
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}

// SourceSchema is the GET /api/sources/{ns}/{app}/schema response. Which
// fields are populated depends on Status: "configured" carries the live
// SchemaYAML; "sampling" (and an in-flight resample) carries the
// SampleCollected/SampleTarget progress.
type SourceSchema struct {
	Status            string `json:"status"`
	SchemaYAML        string `json:"schema_yaml"`
	SchemaHash        string `json:"schema_hash"`
	ResampleRequested bool   `json:"resample_requested"`
	SampleCollected   int    `json:"sample_collected"`
	SampleTarget      int    `json:"sample_target"`
	SampleUpdatedAt   string `json:"sample_updated_at"`
	// Sample carries the sample lines (a JSON array of strings) when the
	// source is ready to (re)configure: a pending first-time sample, or a
	// fresh sample left by a completed resample. Empty otherwise.
	Sample json.RawMessage `json:"sample"`
	// Detection carries the server-computed ingest.Detection (heuristic
	// envelope/JSON detection, plus LLM-classified timestamp/level/trace/
	// span candidates once classification completes — see
	// ingest/llmdetect.go) for whichever sample Sample carries. Present
	// alongside Sample whenever Sample is. The CLI wizard unmarshals this
	// instead of recomputing detection itself — see setup.RunSetupFromLines.
	Detection json.RawMessage `json:"detection"`
	// Preview is the configured schema applied to recent lines, with each
	// extracted value marked; PreviewNote says why there is none (no recent
	// lines).
	Preview     *ingest.Preview `json:"preview,omitempty"`
	PreviewNote string          `json:"preview_note,omitempty"`
	// SDKNote is set when the source is written by the weft SDK: its values
	// come from the line header, so there is no extraction to show.
	SDKNote string `json:"sdk_note,omitempty"`
}

// GetSourceSchema fetches the current schema / sampling state for a source.
func (h *HTTPCoordinator) GetSourceSchema(namespace, service string) (*SourceSchema, error) {
	url := fmt.Sprintf("%s/api/sources/%s/%s/schema", h.baseURL, namespace, service)
	req, _ := http.NewRequest("GET", url, nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpError(resp)
	}
	var sch SourceSchema
	if err := json.NewDecoder(resp.Body).Decode(&sch); err != nil {
		return nil, err
	}
	return &sch, nil
}

// ProbeSource posts a schema-extraction probe request for a source and
// decodes the response into out. The server evaluates it against the source's
// stored sample (see api/sources_probe.go), so in and out are the caller's
// types; this package stays free of the ingest dependency.
func (h *HTTPCoordinator) ProbeSource(namespace, service string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/sources/%s/%s/probe", h.baseURL, namespace, service)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ResampleSource asks the agent for a fresh sample of an already-configured
// source. Ingestion is not interrupted; the fresh sample becomes available
// for review once the agent finishes accumulating. lines is the sample size
// wanted; 0 asks for the default.
func (h *HTTPCoordinator) ResampleSource(namespace, service string, lines int) error {
	url := fmt.Sprintf("%s/api/sources/%s/%s/resample", h.baseURL, namespace, service)
	if lines > 0 {
		url += fmt.Sprintf("?size=%d", lines)
	}
	req, _ := http.NewRequest("POST", url, nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpError(resp)
	}
	return nil
}
