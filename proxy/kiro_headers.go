package proxy

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"omniproxy/config"
)

const (
	kiroStreamingSDKVersion = "1.0.34"
	kiroRuntimeSDKVersion   = "1.0.0"

	// kiro-cli (Rust binary) user-agent strings — required for ksk_ API key auth
	kiroCliUserAgent    = "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.16551 os/linux lang/rust/1.92.0 exec-env/AmazonQ-For-CLI Version/2.8.1 md/appVersion-2.8.1 app/AmazonQ-For-CLI"
	kiroCliAmzUserAgent = "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.16551 os/linux lang/rust/1.92.0 exec-env/AmazonQ-For-CLI Version/2.8.1 m/F app/AmazonQ-For-CLI"
)

type kiroHeaderValues struct {
	UserAgent    string
	AmzUserAgent string
	Host         string
}

func buildStreamingHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererstreaming", kiroStreamingSDKVersion, "m/E")
}

func buildRuntimeHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererruntime", kiroRuntimeSDKVersion, "m/N,E")
}

// machineIdFromAccount derives a deterministic machine fingerprint from the
// account's own credentials — same as 9router's buildKiroFingerprintHeaders.
// Every account gets a unique machineId, and the same account always presents
// the same machineId, so AWS/Kiro sees each account as an independent IDE
// instance. Prevents one account's bad credentials from tainting others.
func machineIdFromAccount(account *config.Account) string {
	seed := ""
	if account != nil {
		// Use most stable unique credential available, same priority as 9router
		seed = account.RefreshToken
		if seed == "" {
			seed = account.AccessToken
		}
	}
	if seed == "" {
		return ""
	}
	h := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%x", h[:16]) // 32 hex chars — enough for uniqueness
}

func buildKiroHeaderValues(account *config.Account, host, apiName, sdkVersion, mode string) kiroHeaderValues {
	clientCfg := config.GetKiroClientConfig()
	// Derive deterministic machine identity from credentials, not from config field
	machineID := machineIdFromAccount(account)

	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/%s#%s %s KiroIDE-%s",
		sdkVersion,
		clientCfg.SystemVersion,
		clientCfg.NodeVersion,
		apiName,
		sdkVersion,
		mode,
		clientCfg.KiroVersion,
	)
	amzUserAgent := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s", sdkVersion, clientCfg.KiroVersion)
	if machineID != "" {
		userAgent += "-" + machineID
		amzUserAgent += "-" + machineID
	}

	return kiroHeaderValues{
		UserAgent:    userAgent,
		AmzUserAgent: amzUserAgent,
		Host:         host,
	}
}

func applyKiroBaseHeaders(req *http.Request, account *config.Account, values kiroHeaderValues) {
	if account != nil && account.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	}
	if account != nil {
		switch account.AuthMethod {
		case "api_key":
			// ALL API keys (including ksk_) require tokentype: API_KEY header.
			// The kiro.dev management/runtime endpoints validate this header.
			req.Header.Set("tokentype", "API_KEY")
		case "external_idp":
			// Enterprise SSO (Azure AD) tokens are IdP-issued JWTs, not AWS Cognito
			// tokens. CodeWhisperer requires this header to recognize the token type;
			// without it the service misparses the audience claim as an ARN.
			req.Header.Set("TokenType", "EXTERNAL_IDP")
		}
	}
	// ksk_ API keys use Smithy-style user-agent matching kiro-cli binary
	if account != nil && strings.HasPrefix(account.AccessToken, "ksk_") {
		req.Header.Set("User-Agent", kiroCliUserAgent)
		req.Header.Set("x-amz-user-agent", kiroCliAmzUserAgent)
	} else {
		req.Header.Set("User-Agent", values.UserAgent)
		req.Header.Set("x-amz-user-agent", values.AmzUserAgent)
	}
	req.Header.Set("x-amzn-codewhisperer-optout", "false")
	if values.Host != "" {
		req.Host = values.Host
	}
}
