package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/graybeardcorp/weft/store"
)

// handleListIntegrations returns the current integration status.
func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}

	nc, _ := s.store.GetNotifyConfig(custID)

	slackConnected := nc != nil && (nc.SlackBotToken != "" || nc.SlackWebhook != "")
	slackInfo := map[string]any{
		"connected": slackConnected,
		"team_name": "",
	}
	if nc != nil && slackConnected {
		slackInfo = map[string]any{
			"connected":     true,
			"team_name":     nc.SlackTeamName,
			"configured_at": nc.ConfiguredAt,
		}
	}

	var pdItems []store.PagerDutyIntegration
	_ = s.store.DB().WithContext(store.WithOrg(context.Background(), custID)).Order("name").Find(&pdItems).Error
	pdKeys := make(map[string]string, len(pdItems))
	pdMeta := make([]map[string]any, 0, len(pdItems))
	for _, it := range pdItems {
		pdKeys[it.Name] = maskKey(it.IntegrationKey)
		pdMeta = append(pdMeta, map[string]any{
			"name":         it.Name,
			"last_used_at": it.LastUsedAt,
			"last_error":   it.LastError,
		})
	}

	json.NewEncoder(w).Encode(map[string]any{
		"slack": slackInfo,
		"pagerduty": map[string]any{
			"connected":    len(pdItems) > 0,
			"keys":         pdKeys,
			"integrations": pdMeta,
		},
	})
}

// handleSlackConnect starts the Slack OAuth flow.
func (s *Server) handleSlackConnect(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	if s.SlackClientID == "" {
		http.Error(w, "Slack integration not configured", http.StatusServiceUnavailable)
		return
	}

	// Generate a state token (signed JWT with customer info).
	stateToken, err := generateStateToken(session.ActiveCustomerName, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	redirectURI := s.apiBaseURL() + "/api/integrations/slack/callback"
	authURL := fmt.Sprintf(
		"https://slack.com/oauth/v2/authorize?client_id=%s&scope=chat:write&redirect_uri=%s&state=%s",
		url.QueryEscape(s.SlackClientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape(stateToken),
	)

	json.NewEncoder(w).Encode(map[string]string{"url": authURL})
}

// handleSlackCallback handles the OAuth callback from Slack.
func (s *Server) handleSlackCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	// Validate state token.
	customerName, err := validateStateToken(state, s.jwtSecret)
	if err != nil {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	// Exchange code for access token.
	redirectURI := s.apiBaseURL() + "/api/integrations/slack/callback"
	tokenResp, err := exchangeSlackCode(code, s.SlackClientID, s.SlackClientSecret, redirectURI)
	if err != nil {
		log.Printf("Slack OAuth exchange error: %v", err)
		http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=error", http.StatusFound)
		return
	}

	// The callback has no session, only the org name carried in the signed
	// state token, so this is a lookup (never a create).
	cust, err := s.store.GetOrgByName(customerName)
	if err != nil {
		http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=error", http.StatusFound)
		return
	}
	custID := cust.ID

	botToken := tokenResp.AccessToken
	teamName := tokenResp.Team.Name
	if botToken == "" {
		log.Printf("Slack OAuth: no access_token in response")
		http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=error", http.StatusFound)
		return
	}

	webhook := ""
	if tokenResp.IncomingWebhook.URL != "" {
		webhook = tokenResp.IncomingWebhook.URL
	}
	s.store.SaveSlackConfig(custID, webhook, botToken, teamName)

	http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=success", http.StatusFound)
}

// handleSlackDisconnect removes the Slack integration.
func (s *Server) handleSlackDisconnect(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	s.store.DB().WithContext(store.WithOrg(context.Background(), custID)).Model(&store.NotifyConfig{}).Updates(map[string]any{
		"slack_webhook":   "",
		"slack_team_name": "",
	})
	w.WriteHeader(http.StatusNoContent)
}

// handlePagerDutySave creates or updates a single named PagerDuty integration.
// Body: {"name": "ops", "key": "<routing-key>", "description": "optional"}.
func (s *Server) handlePagerDutySave(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Key == "" {
		http.Error(w, "name and key required", http.StatusBadRequest)
		return
	}
	var item store.PagerDutyIntegration
	result := s.store.DB().WithContext(store.WithOrg(context.Background(), custID)).Where("name = ?", req.Name).First(&item)
	var saveErr error
	if result.Error == gorm.ErrRecordNotFound {
		saveErr = s.store.DB().WithContext(store.WithOrg(context.Background(), custID)).Create(&store.PagerDutyIntegration{
			OrgID:          custID,
			Name:           req.Name,
			IntegrationKey: req.Key,
		}).Error
	} else {
		saveErr = s.store.DB().WithContext(store.WithOrg(context.Background(), custID)).Model(&item).Update("integration_key", req.Key).Error
	}
	if saveErr != nil {
		http.Error(w, "store error: "+saveErr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handlePagerDutyDelete removes a single integration by name.
// Path: DELETE /api/integrations/pagerduty/{name}
func (s *Server) handlePagerDutyDelete(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	err := s.store.DB().
		WithContext(store.WithOrg(context.Background(), custID)).
		Where("name = ?", name).Delete(&store.PagerDutyIntegration{}).Error
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetNotifyConfig returns the customer's /etc/notify.yaml YAML.
// An absent row yields an empty string — the shell treats that as a blank file.
func (s *Server) handleGetNotifyConfig(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	nc, _ := s.store.GetNotifyConfig(custID)
	yaml := ""
	if nc != nil {
		yaml = nc.AliasesYAML
	}
	json.NewEncoder(w).Encode(map[string]string{"aliases_yaml": yaml})
}

// handleSaveNotifyConfig validates and persists the customer's notify.yaml.
// Validation mirrors the in-process shellfs path so both shells get the same
// errors back (missing targets, bad PD severity, malformed YAML).
func (s *Server) handleSaveNotifyConfig(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	var req struct {
		AliasesYAML string `json:"aliases_yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if _, err := store.ParseNotifyAliases(req.AliasesYAML); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SaveNotifyAliases(custID, session.UserID, req.AliasesYAML); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handlePagerDutyDisconnect removes all PagerDuty integrations for the customer.
func (s *Server) handlePagerDutyDisconnect(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	s.store.DB().WithContext(store.WithOrg(context.Background(), custID)).Delete(&store.PagerDutyIntegration{})
	w.WriteHeader(http.StatusNoContent)
}

// ── Slack OAuth helpers ────────────────────────────────────

type slackOAuthResponse struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
	AccessToken string `json:"access_token"`
	Team        struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"team"`
	IncomingWebhook struct {
		Channel   string `json:"channel"`
		ChannelID string `json:"channel_id"`
		URL       string `json:"url"`
	} `json:"incoming_webhook"`
}

func exchangeSlackCode(code, clientID, clientSecret, redirectURI string) (*slackOAuthResponse, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	}

	resp, err := http.Post(
		"https://slack.com/api/oauth.v2.access",
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result slackOAuthResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse slack response: %w", err)
	}
	if !result.OK {
		return nil, fmt.Errorf("slack oauth error: %s", result.Error)
	}
	return &result, nil
}

// oauthStateTTL is how long a generated OAuth state token is valid.
// Slack's redirect happens within seconds; an aggressive TTL bounds
// any replay window if the token leaks via a referrer or proxy log.
const oauthStateTTL = 10 * time.Minute

// generateStateToken signs a CSRF-style state token that round-trips
// the customer name through the third-party OAuth redirect. It is
// intentionally NOT a JWT — JWTs are for authenticated users, this
// is just an HMAC over (customer, timestamp). Format:
// base64(customer "." unix_ts) "." base64(hmac).
func generateStateToken(customerName, secret string) (string, error) {
	payload := fmt.Sprintf("%s.%d", customerName, time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig, nil
}

// validateStateToken verifies a state token and extracts the customer
// name. Returns an error on bad format, bad signature, or expiry.
func validateStateToken(token, secret string) (string, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid state token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode state token: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return "", fmt.Errorf("invalid state token signature")
	}
	dot := strings.LastIndexByte(string(payload), '.')
	if dot < 0 {
		return "", fmt.Errorf("invalid state token payload")
	}
	customerName := string(payload[:dot])
	var ts int64
	if _, err := fmt.Sscanf(string(payload[dot+1:]), "%d", &ts); err != nil {
		return "", fmt.Errorf("invalid state token timestamp: %w", err)
	}
	if time.Since(time.Unix(ts, 0)) > oauthStateTTL {
		return "", fmt.Errorf("state token expired")
	}
	return customerName, nil
}

// maskKey displays only the first/last few characters of a secret.
func maskKey(key string) string {
	if len(key) > 8 {
		return key[:4] + "..." + key[len(key)-4:]
	}
	return "****"
}
