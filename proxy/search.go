package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"omniproxy/config"
	"strings"
)

const endpointSearch = "search"

// accountHasCapability identifies service accounts without relying on their
// authentication method.  9router service connections use service_api_key,
// while chat connections use external_openai, so AuthMethod alone is not a
// reliable routing signal.
func accountHasCapability(account *config.Account, capability string) bool {
	if account == nil || strings.TrimSpace(capability) == "" {
		return false
	}
	wanted := strings.ToLower(strings.TrimSpace(capability))
	if strings.EqualFold(strings.TrimSpace(account.ProviderKind), wanted) {
		return true
	}
	for _, value := range account.Capabilities {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func isServiceAccount(account *config.Account) bool {
	return accountHasCapability(account, "search") || accountHasCapability(account, "image")
}

type searchRequest struct {
	Query             string   `json:"query"`
	Provider          string   `json:"provider,omitempty"`
	Operation         string   `json:"operation,omitempty"`
	URL               string   `json:"url,omitempty"`
	Topic             string   `json:"topic,omitempty"`
	SearchDepth       string   `json:"search_depth,omitempty"`
	MaxResults        int      `json:"max_results,omitempty"`
	ChunksPerSource   int      `json:"chunks_per_source,omitempty"`
	IncludeAnswer     bool     `json:"include_answer,omitempty"`
	IncludeRawContent bool     `json:"include_raw_content,omitempty"`
	IncludeImages     bool     `json:"include_images,omitempty"`
	IncludeImageDesc  bool     `json:"include_image_descriptions,omitempty"`
	IncludeDomains    []string `json:"include_domains,omitempty"`
	ExcludeDomains    []string `json:"exclude_domains,omitempty"`
	Limit             int      `json:"limit,omitempty"`
}

type normalizedSearchResult struct {
	Title         string  `json:"title,omitempty"`
	URL           string  `json:"url,omitempty"`
	Content       string  `json:"content,omitempty"`
	RawContent    string  `json:"raw_content,omitempty"`
	Score         float64 `json:"score,omitempty"`
	PublishedDate string  `json:"published_date,omitempty"`
}

type normalizedSearchResponse struct {
	Query    string                   `json:"query"`
	Provider string                   `json:"provider"`
	Answer   string                   `json:"answer,omitempty"`
	Results  []normalizedSearchResult `json:"results"`
	Images   []string                 `json:"images,omitempty"`
}

type serviceHTTPError struct {
	Status  int
	Body    string
	Headers map[string]string
}

func (e *serviceHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("upstream service returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("upstream service returned HTTP %d: %s", e.Status, truncateErrBody([]byte(e.Body)))
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON")
		return
	}
	if strings.TrimSpace(req.Query) == "" && strings.TrimSpace(req.URL) == "" {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "query is required")
		return
	}

	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccountID string
	for {
		account := h.pool.GetNextForCapability("search", provider, excluded)
		if account == nil {
			break
		}
		lastAccountID = account.ID
		result, err := callSearchProvider(r, account, req)
		if err == nil {
			h.pool.RecordSuccess(account.ID, "search")
			h.recordUsage(apiKeyIDFromContext(r.Context()), account.ID, "search", endpointSearch, 0, 0, 0, 0, 0, 0)
			writeJSON(w, http.StatusOK, result)
			return
		}
		lastErr = err
		excluded[account.ID] = true
		isQuota := false
		if httpErr, ok := err.(*serviceHTTPError); ok {
			isQuota = httpErr.Status == http.StatusPaymentRequired || httpErr.Status == http.StatusTooManyRequests
		}
		h.pool.RecordError(account.ID, isQuota, "search")
	}

	if lastErr == nil {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available search accounts")
		return
	}
	h.recordError(apiKeyIDFromContext(r.Context()), lastAccountID, "search", endpointSearch, lastErr.Error())
	h.sendOpenAIError(w, serviceErrorStatus(lastErr), "server_error", lastErr.Error())
}

func callSearchProvider(r *http.Request, account *config.Account, req searchRequest) (normalizedSearchResponse, error) {
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	switch provider {
	case "tavily":
		return callTavily(r, account, req)
	case "exa":
		return callExa(r, account, req)
	case "firecrawl":
		return callFirecrawl(r, account, req)
	case "jina-reader":
		return callJinaReader(r, account, req)
	default:
		return normalizedSearchResponse{}, fmt.Errorf("search provider %q is unsupported", account.Provider)
	}
}

func callTavily(r *http.Request, account *config.Account, in searchRequest) (normalizedSearchResponse, error) {
	body := map[string]interface{}{"query": in.Query}
	copySearchOptions(body, in)
	return postSearchJSON(r, account, serviceURL(account, "https://api.tavily.com", "/search"), body, "tavily", func(raw []byte) (normalizedSearchResponse, error) {
		var v struct {
			Query   string                   `json:"query"`
			Answer  string                   `json:"answer"`
			Results []normalizedSearchResult `json:"results"`
			Images  []string                 `json:"images"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return normalizedSearchResponse{}, err
		}
		return normalizedSearchResponse{Query: firstNonEmpty(v.Query, in.Query), Provider: "tavily", Answer: v.Answer, Results: v.Results, Images: v.Images}, nil
	})
}

func callExa(r *http.Request, account *config.Account, in searchRequest) (normalizedSearchResponse, error) {
	body := map[string]interface{}{"query": in.Query}
	if in.NumResults() > 0 {
		body["numResults"] = in.NumResults()
	}
	if in.IncludeRawContent {
		body["contents"] = map[string]interface{}{"text": true}
	}
	return postSearchJSON(r, account, serviceURL(account, "https://api.exa.ai", "/search"), body, "exa", func(raw []byte) (normalizedSearchResponse, error) {
		var v struct {
			Results []struct {
				Title         string  `json:"title"`
				URL           string  `json:"url"`
				Text          string  `json:"text"`
				Score         float64 `json:"score"`
				PublishedDate string  `json:"publishedDate"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return normalizedSearchResponse{}, err
		}
		out := make([]normalizedSearchResult, 0, len(v.Results))
		for _, item := range v.Results {
			out = append(out, normalizedSearchResult{Title: item.Title, URL: item.URL, Content: item.Text, RawContent: item.Text, Score: item.Score, PublishedDate: item.PublishedDate})
		}
		return normalizedSearchResponse{Query: in.Query, Provider: "exa", Results: out}, nil
	})
}

func callFirecrawl(r *http.Request, account *config.Account, in searchRequest) (normalizedSearchResponse, error) {
	body := map[string]interface{}{"query": in.Query}
	if in.Limit > 0 {
		body["limit"] = in.Limit
	} else if in.MaxResults > 0 {
		body["limit"] = in.MaxResults
	}
	return postSearchJSON(r, account, firecrawlSearchURL(account), body, "firecrawl", func(raw []byte) (normalizedSearchResponse, error) {
		var v struct {
			Data []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				Markdown    string `json:"markdown"`
				Content     string `json:"content"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return normalizedSearchResponse{}, err
		}
		out := make([]normalizedSearchResult, 0, len(v.Data))
		for _, item := range v.Data {
			content := firstNonEmpty(item.Markdown, item.Content, item.Description)
			out = append(out, normalizedSearchResult{Title: item.Title, URL: item.URL, Content: content, RawContent: item.Content})
		}
		return normalizedSearchResponse{Query: in.Query, Provider: "firecrawl", Results: out}, nil
	})
}

func firecrawlSearchURL(account *config.Account) string {
	base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if base == "" {
		base = "https://api.firecrawl.dev/v1"
	} else if !strings.HasSuffix(strings.ToLower(base), "/v1") {
		base += "/v1"
	}
	return base + "/search"
}

func callJinaReader(r *http.Request, account *config.Account, in searchRequest) (normalizedSearchResponse, error) {
	target := strings.TrimSpace(in.URL)
	if target == "" {
		target = strings.TrimSpace(in.Query)
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return normalizedSearchResponse{}, fmt.Errorf("jina-reader requires a valid http(s) URL in url or query")
	}
	// Jina Reader's native endpoint is /{target URL}; keep the target scheme
	// because both http:// and https:// are meaningful to the reader.
	endpoint := serviceURL(account, "https://r.jina.ai", "/") + target
	result, err := doServiceRequest(r, account, http.MethodGet, endpoint, nil, "jina-reader")
	if err != nil {
		return normalizedSearchResponse{}, err
	}
	return normalizedSearchResponse{Query: target, Provider: "jina-reader", Results: []normalizedSearchResult{{URL: target, Content: string(result), RawContent: string(result)}}}, nil
}

func postSearchJSON(r *http.Request, account *config.Account, endpoint string, body map[string]interface{}, provider string, normalize func([]byte) (normalizedSearchResponse, error)) (normalizedSearchResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return normalizedSearchResponse{}, err
	}
	raw, err := doServiceRequest(r, account, http.MethodPost, endpoint, payload, provider)
	if err != nil {
		return normalizedSearchResponse{}, err
	}
	return normalize(raw)
}

func doServiceRequest(parent *http.Request, account *config.Account, method, endpoint string, body []byte, provider string) ([]byte, error) {
	request, err := http.NewRequestWithContext(parent.Context(), method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	credential := strings.TrimSpace(account.AccessToken)
	if credential == "" {
		return nil, fmt.Errorf("provider %s has no credential", provider)
	}
	if provider == "exa" {
		request.Header.Set("x-api-key", credential)
	} else {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(request)
	if err != nil {
		_ = config.UpdateAccountServiceStats(account.ID, 0, true, false, nil)
		return nil, err
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if readErr != nil {
		_ = config.UpdateAccountServiceStats(account.ID, response.StatusCode, true, false, serviceResponseHeaders(response.Header))
		return nil, readErr
	}
	headers := serviceResponseHeaders(response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		isQuota := response.StatusCode == http.StatusPaymentRequired || response.StatusCode == http.StatusTooManyRequests
		_ = config.UpdateAccountServiceStats(account.ID, response.StatusCode, true, isQuota, headers)
		return nil, &serviceHTTPError{Status: response.StatusCode, Body: string(raw), Headers: headers}
	}
	_ = config.UpdateAccountServiceStats(account.ID, response.StatusCode, false, false, headers)
	return raw, nil
}

// serviceResponseHeaders keeps only the rate-limit metadata useful to the
// account dashboard. Header names are normalized because providers vary in
// capitalization and spelling conventions.
func serviceResponseHeaders(header http.Header) map[string]string {
	result := make(map[string]string)
	for key, target := range map[string]string{
		"X-RateLimit-Limit":      "limit",
		"X-RateLimit-Remaining":  "remaining",
		"X-RateLimit-Reset":      "reset",
		"Retry-After":            "retry-after",
		"X-Rate-Limit-Limit":     "limit",
		"X-Rate-Limit-Remaining": "remaining",
		"X-Rate-Limit-Reset":     "reset",
	} {
		if value := header.Get(key); value != "" {
			result[target] = value
		}
	}
	return result
}

func copySearchOptions(body map[string]interface{}, in searchRequest) {
	if in.Topic != "" {
		body["topic"] = in.Topic
	}
	if in.SearchDepth != "" {
		body["search_depth"] = in.SearchDepth
	}
	if in.NumResults() > 0 {
		body["max_results"] = in.NumResults()
	}
	if in.ChunksPerSource > 0 {
		body["chunks_per_source"] = in.ChunksPerSource
	}
	if in.IncludeAnswer {
		body["include_answer"] = true
	}
	if in.IncludeRawContent {
		body["include_raw_content"] = true
	}
	if in.IncludeImages {
		body["include_images"] = true
	}
	if in.IncludeImageDesc {
		body["include_image_descriptions"] = true
	}
	if len(in.IncludeDomains) > 0 {
		body["include_domains"] = in.IncludeDomains
	}
	if len(in.ExcludeDomains) > 0 {
		body["exclude_domains"] = in.ExcludeDomains
	}
}

func (in searchRequest) NumResults() int {
	if in.MaxResults > 0 {
		return in.MaxResults
	}
	return in.Limit
}

func serviceURL(account *config.Account, defaultBase, path string) string {
	base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if base == "" {
		base = strings.TrimRight(defaultBase, "/")
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

func serviceErrorStatus(err error) int {
	if _, ok := err.(*unsupportedCapabilityError); ok {
		return http.StatusNotImplemented
	}
	if e, ok := err.(*serviceHTTPError); ok {
		if e.Status == http.StatusTooManyRequests || e.Status >= 500 {
			return http.StatusServiceUnavailable
		}
		return e.Status
	}
	return http.StatusBadGateway
}

func serviceErrorIsQuota(err error) bool {
	e, ok := err.(*serviceHTTPError)
	return ok && (e.Status == http.StatusPaymentRequired || e.Status == http.StatusTooManyRequests)
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
