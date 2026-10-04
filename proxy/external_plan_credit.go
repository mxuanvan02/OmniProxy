package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"omniproxy/config"
	"strconv"
	"strings"
)

// Plan-window credit figures for new-api style gateways.
//
// The subscription row (/api/subscription/self) carries only used_percent — it
// says nothing about how large the plan is. The ceiling lives on two other
// endpoints:
//
//   - /api/subscription/plans -> data[].plan.total_amount, in QUOTA units
//   - /api/status -> quota_per_unit, how many quota units make 1 USD
//
// The console displays "credit", and on VSLLM 1 credit = 1 US cent, pinned two
// independent ways: the marketplace lists qwen3.8-max-0902 at "0.2 credit"
// while the same request bills $0.002, and the top-up page labels a 500000-quota
// plan "100 credit". So credit = quota / quota_per_unit * 100.
//
// Both rates are read from the API rather than hardcoded, because quota_per_unit
// is an operator setting that differs per gateway and the credit-per-USD ratio
// follows from display_in_currency rather than being a constant.

// externalPlanPath is the plan catalog. Like /api/subscription/self it needs the
// console system token + New-Api-User id; without them it answers
// success:false "insufficient privileges".
const externalPlanPath = "/api/subscription/plans"

// externalStatusPath is public (no auth) and carries quota_per_unit.
const externalStatusPath = "/api/status"

// planCatalogEntry is the per-plan data needed to size a subscription window.
type planCatalogEntry struct {
	Title        string
	TotalAmount  int64   // quota units granted per window
	PriceUSD     float64 // what the plan costs to buy (display only)
	ResetSeconds int     // quota_reset_custom_seconds; 0 when period-based
}

// planCatalogEnvelope is the wire shape: data[].plan{...}.
type planCatalogEnvelope struct {
	Data []struct {
		Plan struct {
			ID                     int     `json:"id"`
			Title                  string  `json:"title"`
			TotalAmount            int64   `json:"total_amount"`
			PriceAmount            float64 `json:"price_amount"`
			QuotaResetCustomSecond int     `json:"quota_reset_custom_seconds"`
		} `json:"plan"`
	} `json:"data"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// parsePlanCatalog decodes /api/subscription/plans into a plan-id map. A
// success:false answer is an error, not an empty catalog: empty would silently
// mark every plan unknown and strip all credit ceilings from the UI.
func parsePlanCatalog(body []byte) (map[int]planCatalogEntry, error) {
	var env planCatalogEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode plan catalog: %w", err)
	}
	if !env.Success {
		msg := env.Message
		if msg == "" {
			msg = "success:false"
		}
		return nil, fmt.Errorf("plan catalog refused: %s", msg)
	}
	out := make(map[int]planCatalogEntry, len(env.Data))
	for _, item := range env.Data {
		p := item.Plan
		if p.ID == 0 {
			continue
		}
		out[p.ID] = planCatalogEntry{
			Title:        p.Title,
			TotalAmount:  p.TotalAmount,
			PriceUSD:     p.PriceAmount,
			ResetSeconds: p.QuotaResetCustomSecond,
		}
	}
	return out, nil
}

// parseQuotaPerUnit reads the quota-per-USD rate from /api/status. Returns 0 +
// error when absent or zero, so the caller keeps a previously known rate rather
// than dividing by zero.
func parseQuotaPerUnit(body []byte) (float64, error) {
	var env struct {
		Data struct {
			QuotaPerUnit float64 `json:"quota_per_unit"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, fmt.Errorf("decode status: %w", err)
	}
	if env.Data.QuotaPerUnit <= 0 {
		return 0, fmt.Errorf("quota_per_unit absent or zero")
	}
	return env.Data.QuotaPerUnit, nil
}

// creditsFromQuota converts a quota amount to display credits. 1 credit is one
// US cent on these gateways, so credit = quota / quota_per_unit * 100. Rounded to
// 2 decimal places to match the console and avoid float dust in the UI. Returns 0
// for a non-positive rate so a missing quota_per_unit degrades to "no ceiling"
// instead of dividing by zero or emitting a huge number.
func creditsFromQuota(quota int64, quotaPerUnit float64) float64 {
	if quotaPerUnit <= 0 || quota <= 0 {
		return 0
	}
	credits := float64(quota) / quotaPerUnit * 100.0
	return round2(credits)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// joinPlanCredits folds the plan catalog onto each subscription IN PLACE,
// filling CreditCeiling, CreditUsed, PlanTitle and PlanPriceUSD.
//
// CreditUsed is the ceiling scaled by the subscription's used_percent, so a 78%-
// used 1000-credit plan reads "780 / 1000". A plan the catalog does not list
// (expired, or added after the catalog was cached) leaves the credit fields at
// zero, which the renderer reads as "ceiling unknown" and falls back to the
// percentage bar rather than printing a fabricated total.
//
// Pure and allocation-free apart from rounding: it mutates the caller's slice,
// which is a freshly built snapshot, so no defensive copy is needed.
func joinPlanCredits(subs []config.ExternalSubscription, plans map[int]planCatalogEntry, quotaPerUnit float64) {
	if len(subs) == 0 || len(plans) == 0 || quotaPerUnit <= 0 {
		return
	}
	for i := range subs {
		plan, ok := plans[subs[i].PlanID]
		if !ok {
			continue
		}
		subs[i].PlanTitle = plan.Title
		subs[i].PlanPriceUSD = plan.PriceUSD
		ceiling := creditsFromQuota(plan.TotalAmount, quotaPerUnit)
		subs[i].CreditCeiling = ceiling
		if ceiling > 0 {
			used := round2(ceiling * clampPercent(subs[i].UsedPercent) / 100.0)
			if used > ceiling {
				used = ceiling
			}
			subs[i].CreditUsed = used
		}
	}
}

func clampPercent(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// fetchExternalPlanCatalog reads the plan catalog and the quota-per-USD rate,
// then returns both. The rate is read from the public /api/status; the catalog
// needs the admin credentials, so this is only called when they are present.
//
// A catalog failure is fatal (returns the error) because a snapshot without
// ceilings would regress the UI. A status failure is tolerated: the caller keeps
// the last known quotaPerUnit, since the rate changes far less often than plans.
func fetchExternalPlanCatalog(account *config.Account, fallbackQuotaPerUnit float64) (map[int]planCatalogEntry, float64, error) {
	root := providerRootURL(strings.TrimRight(strings.TrimSpace(account.BaseURL), "/"))
	if root == "" {
		return nil, fallbackQuotaPerUnit, fmt.Errorf("plan catalog: no baseUrl")
	}

	plansBody, err := getExternalAdminJSON(account, root+externalPlanPath)
	if err != nil {
		return nil, fallbackQuotaPerUnit, err
	}
	plans, err := parsePlanCatalog(plansBody)
	if err != nil {
		return nil, fallbackQuotaPerUnit, err
	}

	// The rate is public and best-effort; keep the fallback on any failure.
	qpu := fallbackQuotaPerUnit
	if statusBody, serr := getExternalAdminJSON(account, root+externalStatusPath); serr == nil {
		if parsed, perr := parseQuotaPerUnit(statusBody); perr == nil {
			qpu = parsed
		}
	}
	return plans, qpu, nil
}

// getExternalAdminJSON performs an authenticated GET against a gateway admin
// route using the console system token + New-Api-User id, returning the raw body.
// Shares the credential rules with fetchExternalSubscriptions: the system token
// is sent raw (not Bearer-prefixed) and the inference key is not used.
func getExternalAdminJSON(account *config.Account, url string) ([]byte, error) {
	adminToken := strings.TrimSpace(account.ExtAdminToken)
	if adminToken == "" || account.ExtAdminUserID == 0 {
		return nil, ErrExternalAdminNotConfigured
	}
	req, err := newExternalAdminRequest(account, url)
	if err != nil {
		return nil, err
	}
	resp, err := doExternalOpenAIRequest(GetRestClientForProxy(ResolveAccountProxyURL(account)), req, account)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return readLimitedBody(resp.Body)
}

// newExternalAdminRequest builds a GET against a gateway admin route with the
// credential shape these routes require: the console System Access Token sent
// RAW (not Bearer-prefixed) in Authorization, plus the numeric ExtAdminUserID in
// New-Api-User. This is the single place that knowledge lives — both the
// subscription and the plan-catalog fetchers go through it, so a credential-rule
// change cannot drift between them.
//
// Returns ErrExternalAdminNotConfigured when either credential is absent, so a
// caller that forgets to check still cannot send an unauthenticated request that
// would 401 on every refresh cycle.
func newExternalAdminRequest(account *config.Account, url string) (*http.Request, error) {
	if account == nil {
		return nil, ErrExternalAdminNotConfigured
	}
	adminToken := strings.TrimSpace(account.ExtAdminToken)
	if adminToken == "" || account.ExtAdminUserID == 0 {
		return nil, ErrExternalAdminNotConfigured
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", adminToken) // raw system token, verified live
	req.Header.Set("New-Api-User", strconv.Itoa(account.ExtAdminUserID))
	req.Header.Set("User-Agent", externalOpenAIUserAgent)
	return req, nil
}

// readLimitedBody reads a response body capped at 1 MiB, the same bound the other
// external JSON readers use, so a gateway that returns an HTML error page cannot
// make us buffer unbounded bytes.
func readLimitedBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, 1<<20))
}
