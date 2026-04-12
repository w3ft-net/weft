package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/kevlarcon/weft/store"
)

// handleListIntegrations returns the current integration status.
func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nc, _ := s.store.GetNotifyConfig(custID)

	resp := map[string]any{
		"slack": map[string]any{
			"connected": nc != nil && nc.SlackWebhook != "",
			"team_name": "",
		},
		"pagerduty": map[string]any{
			"connected": nc != nil && nc.PagerDutyKeys != "" && nc.PagerDutyKeys != "{}",
			"keys":      map[string]string{},
		},
		"routes": map[string]string{},
	}

	if nc != nil {
		if nc.SlackWebhook != "" {
			resp["slack"] = map[string]any{
				"connected":     true,
				"team_name":     nc.SlackTeamName,
				"configured_at": nc.ConfiguredAt,
			}
		}
		if nc.PagerDutyKeys != "" {
			var keys map[string]string
			json.Unmarshal([]byte(nc.PagerDutyKeys), &keys)
			resp["pagerduty"] = map[string]any{
				"connected": len(keys) > 0,
				"keys":      maskKeys(keys),
			}
		}
		if nc.Routes != "" {
			var routes map[string]string
			json.Unmarshal([]byte(nc.Routes), &routes)
			resp["routes"] = routes
		}
	}

	json.NewEncoder(w).Encode(resp)
}

// handleSlackConnect starts the Slack OAuth flow.
func (s *Server) handleSlackConnect(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)

	if s.SlackClientID == "" {
		http.Error(w, "Slack integration not configured", http.StatusServiceUnavailable)
		return
	}

	// Generate a state token (signed JWT with customer info).
	stateToken, err := generateStateToken(claims.CustomerName, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	redirectURI := s.baseURL + "/api/integrations/slack/callback"
	authURL := fmt.Sprintf(
		"https://slack.com/oauth/v2/authorize?client_id=%s&scope=incoming-webhook&redirect_uri=%s&state=%s",
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
	redirectURI := s.baseURL + "/api/integrations/slack/callback"
	tokenResp, err := exchangeSlackCode(code, s.SlackClientID, s.SlackClientSecret, redirectURI)
	if err != nil {
		log.Printf("Slack OAuth exchange error: %v", err)
		http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=error", http.StatusFound)
		return
	}

	// Save the webhook.
	custID, err := s.resolveCustomerID(customerName)
	if err != nil {
		http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=error", http.StatusFound)
		return
	}

	webhook := tokenResp.IncomingWebhook.URL
	teamName := tokenResp.Team.Name
	if webhook == "" {
		log.Printf("Slack OAuth: no webhook in response")
		http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=error", http.StatusFound)
		return
	}

	s.store.SaveSlackWebhook(custID, webhook, teamName)

	http.Redirect(w, r, s.baseURL+"/account?integration=slack&status=success", http.StatusFound)
}

// handleSlackDisconnect removes the Slack integration.
func (s *Server) handleSlackDisconnect(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.store.DisconnectSlack(custID)
	w.WriteHeader(http.StatusNoContent)
}

// handlePagerDutySave saves a PagerDuty routing key.
func (s *Server) handlePagerDutySave(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var req struct {
		Name string `json:"name"` // e.g., "api", "workers"
		Key  string `json:"key"`  // PagerDuty routing key
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Key == "" {
		http.Error(w, "name and key required", http.StatusBadRequest)
		return
	}

	// Load existing keys, add/update this one.
	nc, _ := s.store.GetNotifyConfig(custID)
	keys := make(map[string]string)
	if nc != nil && nc.PagerDutyKeys != "" {
		json.Unmarshal([]byte(nc.PagerDutyKeys), &keys)
	}
	keys[req.Name] = req.Key

	keysJSON, _ := json.Marshal(keys)
	s.store.SavePagerDutyKeys(custID, string(keysJSON))

	w.WriteHeader(http.StatusOK)
}

// handlePagerDutyDisconnect removes all PagerDuty keys.
func (s *Server) handlePagerDutyDisconnect(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.store.SavePagerDutyKeys(custID, "{}")
	w.WriteHeader(http.StatusNoContent)
}

// handleSaveRoutes saves the watch→destination routing map.
func (s *Server) handleSaveRoutes(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var routes map[string]string
	if err := json.NewDecoder(r.Body).Decode(&routes); err != nil {
		http.Error(w, "invalid routes", http.StatusBadRequest)
		return
	}

	routesJSON, _ := json.Marshal(routes)
	s.store.SaveNotifyRoutes(custID, string(routesJSON))

	w.WriteHeader(http.StatusOK)
}

// ── Slack OAuth helpers ──────────────���──────────────────────

type slackOAuthResponse struct {
	OK              bool   `json:"ok"`
	Error           string `json:"error"`
	AccessToken     string `json:"access_token"`
	Team            struct {
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

// generateStateToken creates a signed token containing the customer name.
func generateStateToken(customerName, secret string) (string, error) {
	user := &store.User{CustomerName: customerName}
	return GenerateJWT(user, "oauth-state", secret)
}

// validateStateToken validates and extracts the customer name from a state token.
func validateStateToken(token, secret string) (string, error) {
	claims, err := ValidateJWT(token, secret)
	if err != nil {
		return "", err
	}
	return claims.CustomerName, nil
}

// maskKeys replaces key values with masked versions for display.
func maskKeys(keys map[string]string) map[string]string {
	masked := make(map[string]string, len(keys))
	for name, key := range keys {
		if len(key) > 8 {
			masked[name] = key[:4] + "..." + key[len(key)-4:]
		} else {
			masked[name] = "****"
		}
	}
	return masked
}
