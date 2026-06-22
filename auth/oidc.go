package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"superkiro/config"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oidcTokenURL builds the idc/builderId refresh endpoint. Replacable in tests.
var oidcTokenURL = func(region string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
}

// socialTokenURL builds the social refresh endpoint. Replacable in tests.
var socialTokenURL = func() string {
	return "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
}

// socialDeviceAuthURL is the social device authorization endpoint. Replacable in tests.
var socialDeviceAuthURL = func() string {
	return "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/device/authorization"
}

// socialDevicePollURL is the social device poll endpoint. Replacable in tests.
var socialDevicePollURL = func() string {
	return "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/device/poll"
}

// SocialDeviceSession stores state for a social device-code login.
type SocialDeviceSession struct {
	DeviceCode string
	UserCode   string
	VerifyURL  string
	Interval   int
	ExpiresAt  time.Time
	Provider   string
}

// StartSocialLogin initiates a Google/GitHub social device-code login.
// Returns session with deviceCode, userCode, verification URL, and poll interval.
func StartSocialLogin(provider string) (*SocialDeviceSession, error) {
	idp := "Google"
	if provider == "github" {
		idp = "Github"
	}

	payload := map[string]string{
		"clientId":      "kiro-cli",
		"loginProvider": idp,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", socialDeviceAuthURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorization failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device authorization failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationUriComplete string `json:"verificationUriComplete"`
		IntervalMS              int    `json:"intervalInMilliseconds"`
		ExpiresInMS             int    `json:"expiresInMilliseconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse response failed: %w", err)
	}

	interval := result.IntervalMS / 1000
	if interval < 1 {
		interval = 5
	}
	expiresIn := result.ExpiresInMS / 1000
	if expiresIn < 1 {
		expiresIn = 300
	}

	return &SocialDeviceSession{
		DeviceCode: result.DeviceCode,
		UserCode:   result.UserCode,
		VerifyURL:  result.VerificationUriComplete,
		Interval:   interval,
		ExpiresAt:  time.Now().Add(time.Duration(expiresIn) * time.Second),
		Provider:   provider,
	}, nil
}

// PollSocialLogin polls the social device-code flow for tokens.
// Returns accessToken, refreshToken, profileArn, expiresIn, error.
// profileArn comes directly from the kiro.dev response.
func PollSocialLogin(deviceCode, provider string) (accessToken, refreshToken, profileArn string, expiresIn int, err error) {
	payload := map[string]string{
		"deviceCode": deviceCode,
		"clientId":   "kiro-cli",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", socialDevicePollURL(), bytes.NewReader(body))
	if err != nil {
		return "", "", "", 0, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("poll request failed: %w", err)
	}
	defer resp.Body.Close()

	// kiro.dev returns 400 with {error:"authorization_pending"} when pending.
	// 200 only comes when tokens are ready.
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileArn   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", "", 0, fmt.Errorf("parse response failed: %w", err)
	}

	if result.Error == "authorization_pending" || result.Error == "slow_down" || result.Error == "no_tokens" {
		return "", "", "", 0, fmt.Errorf(result.Error)
	}
	if result.Error != "" {
		return "", "", "", 0, fmt.Errorf("authorization error: %s", result.Error)
	}
	if result.AccessToken == "" && resp.StatusCode != 200 {
		return "", "", "", 0, fmt.Errorf("authorization_pending")
	}
	if result.AccessToken == "" {
		return "", "", "", 0, fmt.Errorf("authorization_pending")
	}

	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 3600
	}

	return result.AccessToken, result.RefreshToken, result.ProfileArn, result.ExpiresIn, nil
}

// DiscoverProfileArn discovers the CodeWhisperer/Kiro profile ARN for a newly-authenticated
// account by calling ListAvailableProfiles on the region-matched Amazon Q endpoint. Prefers a
// profile whose ARN contains the token's region, then falls back to the first profile returned.
// Builder ID accounts legitimately have none and return "" without error.
// When externalIdp is true, includes TokenType: EXTERNAL_IDP for enterprise SSO tokens.
// kiroProfileHost returns the CodeWhisperer/Q service host for a region.
// The legacy `codewhisperer.<region>` host only exists in us-east-1; every other
// region (e.g. eu-central-1) is served by `q.<region>.amazonaws.com`.
func kiroProfileHost(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" || region == "us-east-1" {
		return "https://codewhisperer.us-east-1.amazonaws.com"
	}
	return fmt.Sprintf("https://q.%s.amazonaws.com", region)
}

func DiscoverProfileArn(accessToken, region string, externalIdp bool) string {
	if accessToken == "" {
		return ""
	}
	if region == "" {
		region = "us-east-1"
	}
	region = strings.ToLower(region)

	runtimeHost := kiroProfileHost(region)

	payload := map[string]int{"maxResults": 10}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", runtimeHost+"/", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	machineID := deriveMachineID(accessToken, region)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Accept", "application/x-amz-json-1.0")
	req.Header.Set("x-amz-target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("amz-sdk-invocation-id", machineID)
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("User-Agent", fmt.Sprintf("aws-sdk-js/1.0.0 ua/2.1 os/windows#10.0.26200 lang/js md/nodejs#22.21.1 api/codewhispererruntime#1.0.0 m/N,E KiroIDE-0.10.32-%s", machineID))
	req.Header.Set("x-amz-user-agent", fmt.Sprintf("aws-sdk-js/1.0.0 KiroIDE-0.10.32-%s", machineID))
	if externalIdp {
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	}

	client := httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return ""
	}

	var result struct {
		Profiles []struct {
			Arn string `json:"arn"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	// Prefer region-matched profile.
	normalizedRegion := strings.ToLower(region)
	var fallback string
	for _, profile := range result.Profiles {
		arn := strings.TrimSpace(profile.Arn)
		if arn == "" {
			continue
		}
		if strings.Contains(strings.ToLower(arn), ":"+normalizedRegion+":") {
			return arn
		}
		if fallback == "" {
			fallback = arn
		}
	}
	return fallback
}

// kiroProfileRegions lists the AWS regions where the Amazon Q Developer profile
// (and its service endpoint q.<region>.amazonaws.com) actually exists. Per AWS
// docs the Q Developer profile is only supported in us-east-1 and eu-central-1,
// and the IAM Identity Center (SSO) region may differ from the profile region —
// so an account is probed across both to locate its profile.
// Ref: https://docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/q-admin-setup-subscribe-regions.html
var kiroProfileRegions = []string{
	"us-east-1", "eu-central-1",
}

// DiscoverProfileArnMultiRegion probes Kiro/Q service endpoints across regions and
// returns the first profile ARN found. preferredRegion (the account's SSO region)
// is tried first. Returns "" when no profile is available in any region.
func DiscoverProfileArnMultiRegion(accessToken, preferredRegion string, externalIdp bool) string {
	if accessToken == "" {
		return ""
	}
	seen := map[string]bool{}
	var regions []string
	add := func(r string) {
		r = strings.ToLower(strings.TrimSpace(r))
		if r != "" && !seen[r] {
			seen[r] = true
			regions = append(regions, r)
		}
	}
	add(preferredRegion)
	for _, r := range kiroProfileRegions {
		add(r)
	}
	for _, r := range regions {
		if arn := DiscoverProfileArn(accessToken, r, externalIdp); arn != "" {
			return arn
		}
	}
	return ""
}

// deriveMachineID builds a stable machine identifier from the access token and region,
// matching the reference implementation's BuildMachineID.
func deriveMachineID(parts ...string) string {
	seed := strings.Join(parts, "|")
	// Simple hash for machine ID — not crypto-critical, just needs to be stable.
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(seed)))
	return h[:16]
}

// ListProfiles calls the CodeWhisperer ListAvailableProfiles endpoint with the Kiro IDE
// headers. When externalIdp is true, includes TokenType: EXTERNAL_IDP for enterprise SSO.
// Returns the first profile ARN or "" if none available.
func ListProfiles(accessToken, region string, externalIdp bool) string {
	if accessToken == "" {
		return ""
	}
	if region == "" {
		region = "us-east-1"
	}
	region = strings.ToLower(region)

	runtimeHost := fmt.Sprintf("https://codewhisperer.%s.amazonaws.com", region)

	payload := map[string]int{"maxResults": 10}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", runtimeHost+"/", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	machineID := deriveMachineID(accessToken, region)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Accept", "application/x-amz-json-1.0")
	req.Header.Set("x-amz-target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("amz-sdk-invocation-id", machineID)
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("User-Agent", fmt.Sprintf("aws-sdk-js/1.0.0 ua/2.1 os/windows#10.0.26200 lang/js md/nodejs#22.21.1 api/codewhispererruntime#1.0.0 m/N,E KiroIDE-0.10.32-%s", machineID))
	req.Header.Set("x-amz-user-agent", fmt.Sprintf("aws-sdk-js/1.0.0 KiroIDE-0.10.32-%s", machineID))
	if externalIdp {
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	}

	client := httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return ""
	}

	var result struct {
		Profiles []struct {
			Arn string `json:"arn"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	normalizedRegion := strings.ToLower(region)
	var fallback string
	for _, profile := range result.Profiles {
		arn := strings.TrimSpace(profile.Arn)
		if arn == "" {
			continue
		}
		if strings.Contains(strings.ToLower(arn), ":"+normalizedRegion+":") {
			return arn
		}
		if fallback == "" {
			fallback = arn
		}
	}
	return fallback
}

// ResolveProfileArn attempts to discover the profile ARN for a credential. If the initial
// ListProfiles call fails, it refreshes the token (when clientID/clientSecret are available)
// and retries. Returns the resolved profile ARN and optionally refreshed token values.
func ResolveProfileArn(accessToken, region, clientID, clientSecret, tokenEndpoint, scopes string, currentRefreshToken string) (profileArn string, newAccessToken, newRefreshToken string, newExpiresAt int64, err error) {
	if accessToken == "" {
		return "", "", "", 0, fmt.Errorf("resolve profile: access token is empty")
	}
	if region == "" {
		region = "us-east-1"
	}

	externalIdp := tokenEndpoint != ""
	profileArn = ListProfiles(accessToken, region, externalIdp)
	if profileArn != "" {
		return profileArn, "", "", 0, nil
	}

	// First call returned nothing — try refresh then retry.
	if clientID == "" || clientSecret == "" {
		return "", "", "", 0, fmt.Errorf("resolve profile: no profiles found and no client credentials to refresh")
	}

	// Build a temp account for refresh. Use the OIDC method for device-flow tokens;
	authMethod := "idc"
	if externalIdp {
		authMethod = "external_idp"
	}
	newAT, newRT, newExp, pa, _, _, refreshErr := RefreshToken(&config.Account{
		AccessToken:   accessToken,
		RefreshToken:  currentRefreshToken,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		AuthMethod:    authMethod,
		Region:        region,
		TokenEndpoint: tokenEndpoint,
		Scopes:        scopes,
	})
	if refreshErr != nil {
		return "", "", "", 0, fmt.Errorf("resolve profile: refresh failed: %w", refreshErr)
	}

	// After refresh the social path may have returned a profileArn.
	if pa != "" {
		return pa, newAT, newRT, newExp, nil
	}

	// Retry ListProfiles with refreshed token.
	if newAT != "" {
		pa2 := ListProfiles(newAT, region, externalIdp)
		if pa2 != "" {
			return pa2, newAT, newRT, newExp, nil
		}
	}

	return "", newAT, newRT, newExp, fmt.Errorf("resolve profile: no profiles available after refresh")
}

// CachedProfileArnForStartURL looks for an existing account with the same start URL and
// region that already has a profileArn. This is used as a last-resort fallback for IDC
// tenants where ListAvailableProfiles returns empty after a successful device login.
func CachedProfileArnForStartURL(startURL, region string) string {
	if region == "" {
		region = "us-east-1"
	}
	region = strings.ToLower(strings.TrimSpace(region))
	startURL = strings.TrimSpace(startURL)
	if startURL == "" {
		return ""
	}

	accounts := config.GetAccounts()
	for _, a := range accounts {
		if a.ProfileArn == "" {
			continue
		}
		if strings.TrimSpace(a.StartUrl) != startURL {
			continue
		}
		if strings.ToLower(strings.TrimSpace(a.Region)) != region {
			continue
		}
		return a.ProfileArn
	}
	return ""
}

// RefreshToken refreshes the access token
// Returns: accessToken, refreshToken, expiresAt, profileArn, error
// RefreshToken refreshes the access token.
// Returns: accessToken, refreshToken, expiresAt, profileArn, newClientID, newClientSecret, error.
// newClientID/newClientSecret are set when the OIDC client was re-registered during refresh.
func RefreshToken(account *config.Account) (string, string, int64, string, string, string, error) {
	// Resolve per-account proxy: account.ProxyURL > global config
	proxyURL := account.ProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	client := GetAuthClientForProxy(proxyURL)

	// External IdP (enterprise SSO) refresh against the IdP's own token endpoint.
	// The IdP-issued token is not valid at the Kiro OIDC or social endpoints.
	if account.AuthMethod == "external_idp" && account.TokenEndpoint != "" {
		at, rt, exp, err := refreshViaExternalIdp(
			account.RefreshToken, account.TokenEndpoint, account.ClientID, account.Scopes, client,
		)
		if err == nil {
			return at, rt, exp, "", "", "", nil
		}
		// Fall through to social fallback as last resort.
	}

	if account.AuthMethod == "social" {
		// Try social refresh first (kiro.dev/refreshToken).
		at, rt, exp, pa, err := refreshSocialToken(account.RefreshToken, client)
		if err == nil {
			return at, rt, exp, pa, "", "", nil
		}
		// Social refresh failed. For Google social tokens, the kiro.dev refresh
		// endpoint may not accept them. Try OIDC refresh with a freshly registered
		// client (same fallback uses for OIDC accounts).
		region := account.Region
		if region == "" {
			region = "us-east-1"
		}
		newClientID, newClientSecret, regErr := RegisterOIDCClient(region)
		if regErr == nil && newClientID != "" {
			at, rt, exp, pa, oidcErr := refreshOIDCToken(
				account.RefreshToken, newClientID, newClientSecret, region, client,
			)
			if oidcErr == nil {
				return at, rt, exp, pa, newClientID, newClientSecret, nil
			}
		}
		return "", "", 0, "", "", "", err
	}

	// OIDC refresh first (Builder ID / IDC).
	accessToken, refreshToken, expiresAt, profileArn, err := refreshOIDCToken(
		account.RefreshToken, account.ClientID, account.ClientSecret, account.Region, client,
	)
	if err == nil {
		return accessToken, refreshToken, expiresAt, profileArn, "", "", nil
	}

	// OIDC refresh failed. Try re-registering the OIDC client (client credentials
	// may have expired) and retry once.
	region := account.Region
	if region == "" {
		region = "us-east-1"
	}
	newClientID, newClientSecret, regErr := RegisterOIDCClient(region)
	if regErr == nil && newClientID != "" {
		accessToken, refreshToken, expiresAt, profileArn, err = refreshOIDCToken(
			account.RefreshToken, newClientID, newClientSecret, region, client,
		)
		if err == nil {
			return accessToken, refreshToken, expiresAt, profileArn, newClientID, newClientSecret, nil
		}
	}

	// Final fallback: try social refresh endpoint. Kiro.dev social refresh works
	// for any valid refresh token regardless of auth method.
	socAT, socRT, socExp, socProfile, socErr := refreshSocialToken(account.RefreshToken, client)
	if socErr == nil && socAT != "" {
		return socAT, socRT, socExp, socProfile, "", "", nil
	}

	return "", "", 0, "", "", "", err
}

// RegisterOIDCClient registers a new OIDC client pair with AWS SSO.
func RegisterOIDCClient(region string) (clientID, clientSecret string, err error) {
	oidcBase := fmt.Sprintf("https://oidc.%s.amazonaws.com", region)
	startUrl := "https://view.awsapps.com/start"
	scopes := []string{
		"codewhisperer:completions",
		"codewhisperer:analysis",
		"codewhisperer:conversations",
		"codewhisperer:transformations",
		"codewhisperer:taskassist",
	}
	payload := map[string]interface{}{
		"clientName": "Kiro",
		"clientType": "public",
		"scopes":     scopes,
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  startUrl,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", oidcBase+"/client/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	client := httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("register client failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("register client failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("parse register response: %w", err)
	}
	return result.ClientID, result.ClientSecret, nil
}

// refreshOIDCToken refreshes IdC/Builder ID token
func refreshOIDCToken(refreshToken, clientID, clientSecret, region string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || clientSecret == "" {
		return "", "", 0, "", fmt.Errorf("OIDC refresh requires clientId and clientSecret")
	}
	if region == "" {
		region = "us-east-1"
	}

	url := oidcTokenURL(region)

	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}

// refreshViaExternalIdp refreshes an enterprise IdP-issued token through the IdP's own
// token endpoint (form-encoded OAuth2 refresh_token grant, snake_case response).
func refreshViaExternalIdp(refreshToken, tokenEndpoint, clientID, scopes string, client *http.Client) (string, string, int64, error) {
	if refreshToken == "" || tokenEndpoint == "" {
		return "", "", 0, fmt.Errorf("external IdP refresh: refresh token and endpoint required")
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if strings.TrimSpace(scopes) != "" {
		form.Set("scope", scopes)
	}
	req, err := http.NewRequest("POST", tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP refresh: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP refresh: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, fmt.Errorf("external IdP refresh failed (status %d): %s", resp.StatusCode, string(body))
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &result)
	if result.AccessToken == "" {
		if result.Error != "" {
			return "", "", 0, fmt.Errorf("external IdP refresh: %s", result.Error)
		}
		return "", "", 0, fmt.Errorf("external IdP refresh: empty access token")
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 3600
	}
	return result.AccessToken, result.RefreshToken, time.Now().Unix()+int64(result.ExpiresIn), nil
}

// refreshSocialToken refreshes Social (GitHub/Google) token
func refreshSocialToken(refreshToken string, client *http.Client) (string, string, int64, string, error) {
	url := socialTokenURL()

	payload := map[string]string{
		"refreshToken": refreshToken,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}
