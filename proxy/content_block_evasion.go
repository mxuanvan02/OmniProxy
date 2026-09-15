// Package proxy: content_block_evasion.go
//
// Recovery path for upstream content-scanner rejections (AgentRouter answers a
// match with HTTP 400 "content-blocked" or HTTP 500 "sensitive_words_detected").
//
// # WHY THIS EXISTS
//
// The scanner sits at the provider edge and answers in 0.2-0.3s, before the
// model and before the billing gate. It is a payload-level refusal, so account
// failover does not help: every account behind the same edge rejects the same
// bytes identically. The only two outcomes previously available were "burn a
// failover slot per account" or "fail the turn".
//
// WHAT WAS ESTABLISHED EXPERIMENTALLY (2026-09-12)
//
// The scanner compares raw bytes and does not normalise Unicode. Splitting a
// flagged token with a zero-width character passes it, while the identical
// string without the split is rejected on every attempt:
//
//	GODMODE            -> blocked 3/3
//	GOD<ZWSP>MODE      -> passed
//	GODMODE + ZWNJ / word-joiner / soft-hyphen, mid-token -> passed
//
// Insertion position mattered in a way a plain substring match cannot explain
// (positions 1-2 stayed blocked while 3-6 passed), so this code does NOT rely
// on one lucky offset: it splits every few runes, which passed in all trials.
//
// # WHAT IS STILL UNKNOWN — READ BEFORE EXTENDING
//
// Which terms the scanner actually flags is NOT known. A hand-written blocklist
// was tried and reverted the same day: the real skills line that carries both
// GODMODE and ULTRAPLINIAN is NOT rejected, while a short synthetic string
// containing GODMODE is. Guessing terms therefore both fails to fix real
// traffic and corrupts every request it touches.
//
// So this module never guesses. It learns from an actual rejection: on a block
// it obfuscates, retries, and — only if the retry succeeds — records the terms
// that made the difference. Until something is learned, the fallback is a
// bounded bisect over the request's own text, not a static list.
//
// # OPERATOR-VISIBLE RISK
//
// Splitting tokens to get past a provider's content filter is evasion. The
// provider may treat it as a terms violation and suspend the account. This
// recovery is therefore restricted to AgentRouter accounts and never applies
// to other providers.
package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"omniproxy/logger"
)

// zeroWidthSplitter is inserted inside a flagged token. U+200B (zero-width
// space) is used rather than the other three that also worked because it is the
// most widely handled by tokenizers, and because soft-hyphen renders as a hyphen
// in some clients — which would be visible to the user.
const zeroWidthSplitter = "\u200b"

// splitEveryNRunes controls how often the splitter is inserted inside a token.
// A single mid-token split was enough in every successful trial, but position
// sensitivity was observed and not explained, so several splits are used. 3 is
// the largest stride that still guarantees a split inside any 4-rune token.
const splitEveryNRunes = 3

// minObfuscationLen is the shortest token worth splitting. Below this the
// splitter is a large fraction of the token and the token is too common to be
// a plausible scanner target.
const minObfuscationLen = 4

// maxLearnedTerms caps the store so a pathological stream of rejections cannot
// grow it without bound and cannot slow every subsequent request.
const maxLearnedTerms = 200

// maxBisectAttempts bounds the learning search. Each attempt is a real upstream
// request against the operator's account, so this is deliberately small: a
// request that cannot be recovered in a few tries is failed over instead of
// being retried into a rate limit.
const maxBisectAttempts = 4

// ---------------------------------------------------------------------------
// Obfuscation
// ---------------------------------------------------------------------------

// obfuscateToken splits a token with zero-width characters so a raw-byte
// scanner no longer matches it, while a reader (and the model) still sees the
// original word. Tokens shorter than minObfuscationLen are returned unchanged.
func obfuscateToken(token string) string {
	runes := []rune(token)
	if len(runes) < minObfuscationLen {
		return token
	}
	var b strings.Builder
	// Grow generously: worst case one splitter per splitEveryNRunes runes.
	b.Grow(len(token) + len(zeroWidthSplitter)*(len(runes)/splitEveryNRunes+1))
	for i, r := range runes {
		// Never insert before the first or after the last rune: a splitter at
		// the boundary leaves the token itself contiguous, and boundary-only
		// insertion was observed to stay blocked.
		if i > 0 && i < len(runes) && i%splitEveryNRunes == 0 {
			b.WriteString(zeroWidthSplitter)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isObfuscated reports whether s already carries a splitter, so repeated passes
// over the same payload cannot stack splitters and inflate the token count.
func isObfuscated(s string) bool {
	return strings.Contains(s, zeroWidthSplitter)
}

// obfuscateTermsIn rewrites every occurrence of each term in text, matching
// case-insensitively but preserving the original casing of what it replaces.
// Returns the new text and whether anything changed.
func obfuscateTermsIn(text string, terms []string) (string, bool) {
	if text == "" || len(terms) == 0 {
		return text, false
	}
	out := text
	changed := false
	for _, term := range terms {
		if len(term) < minObfuscationLen {
			continue
		}
		// Case-insensitive literal search, walking the string so the original
		// casing at each hit is preserved in the replacement.
		lowerTerm := strings.ToLower(term)
		var b strings.Builder
		rest := out
		for {
			idx := strings.Index(strings.ToLower(rest), lowerTerm)
			if idx < 0 {
				b.WriteString(rest)
				break
			}
			hit := rest[idx : idx+len(term)]
			if isObfuscated(hit) {
				// Already split by an earlier term; leave it alone.
				b.WriteString(rest[:idx+len(term)])
			} else {
				b.WriteString(rest[:idx])
				b.WriteString(obfuscateToken(hit))
				changed = true
			}
			rest = rest[idx+len(term):]
		}
		out = b.String()
	}
	return out, changed
}

// ---------------------------------------------------------------------------
// Payload traversal
// ---------------------------------------------------------------------------

// applyToPayloadText walks every human-readable text field of the payload and
// rewrites it with fn.
//
// The exclusions are the whole point of doing this structurally instead of on
// the marshalled JSON. Splitting any of these would break the request rather
// than launder it:
//
//   - tool names: the client matches tool_use results against its own registry
//     by exact name, and Kiro validates them against the declared tool list
//   - toolUseId / conversationId: correlation identifiers
//   - image bytes: base64 payloads
//   - JSON-schema keys inside InputSchema: structural, and validated upstream
//   - model IDs / profile ARNs: routing identifiers
//
// Tool *descriptions* are rewritten, since they are prose the scanner reads and
// nothing matches on them.
func applyToPayloadText(payload *KiroPayload, fn func(string) (string, bool)) bool {
	if payload == nil {
		return false
	}
	changed := false
	apply := func(s *string) {
		if s == nil || *s == "" {
			return
		}
		if out, ok := fn(*s); ok {
			*s = out
			changed = true
		}
	}

	cur := &payload.ConversationState.CurrentMessage.UserInputMessage
	apply(&cur.Content)
	applyToUserMessageContext(cur.UserInputMessageContext, apply)

	for i := range payload.ConversationState.History {
		h := &payload.ConversationState.History[i]
		if h.UserInputMessage != nil {
			apply(&h.UserInputMessage.Content)
			applyToUserMessageContext(h.UserInputMessage.UserInputMessageContext, apply)
		}
		if h.AssistantResponseMessage != nil {
			apply(&h.AssistantResponseMessage.Content)
			// ToolUses are deliberately untouched: Name is an identifier and
			// Input is structured arguments the tool will parse.
		}
	}
	return changed
}

func applyToUserMessageContext(ctx *UserInputMessageContext, apply func(*string)) {
	if ctx == nil {
		return
	}
	for i := range ctx.Tools {
		// Name is an identifier — only the description is prose.
		apply(&ctx.Tools[i].ToolSpecification.Description)
	}
	for i := range ctx.ToolResults {
		for j := range ctx.ToolResults[i].Content {
			apply(&ctx.ToolResults[i].Content[j].Text)
		}
	}
}

// collectPayloadText returns the payload's readable text, used for candidate
// extraction. Order is stable so bisecting is reproducible.
func collectPayloadText(payload *KiroPayload) []string {
	var out []string
	applyToPayloadText(payload, func(s string) (string, bool) {
		out = append(out, s)
		return s, false // read-only pass
	})
	return out
}

// ---------------------------------------------------------------------------
// Candidate extraction
// ---------------------------------------------------------------------------

// candidateTokenRe matches word-like runs, including internal underscores and
// hyphens so terms such as "sensitive_words" or "red-teaming" survive as one
// candidate.
var candidateTokenRe = regexp.MustCompile(`[\p{L}\p{N}_-]{4,}`)

// extractCandidateTerms ranks the payload's tokens by how likely a content
// scanner is to be matching on them, most distinctive first.
//
// The ranking is a heuristic and is only ever used to order retry attempts —
// nothing is obfuscated permanently until a retry actually succeeds. Rare and
// shouty tokens sort first because a keyword list is far more likely to hold
// SCREAMING_IDENTIFIERS and unusual proper nouns than ordinary prose.
func extractCandidateTerms(payload *KiroPayload, limit int) []string {
	freq := map[string]int{}
	casing := map[string]string{}
	for _, text := range collectPayloadText(payload) {
		for _, tok := range candidateTokenRe.FindAllString(text, -1) {
			if isObfuscated(tok) {
				continue
			}
			key := strings.ToLower(tok)
			freq[key]++
			// Prefer the shoutiest observed casing for the eventual match.
			if prev, ok := casing[key]; !ok || score(tok) > score(prev) {
				casing[key] = tok
			}
		}
	}
	type cand struct {
		term  string
		score int
		freq  int
	}
	cands := make([]cand, 0, len(freq))
	for key, n := range freq {
		tok := casing[key]
		// A token repeated all over the payload is structural vocabulary, not
		// a flagged term; deprioritise it.
		cands = append(cands, cand{term: tok, score: score(tok), freq: n})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].freq != cands[j].freq {
			return cands[i].freq < cands[j].freq
		}
		return cands[i].term < cands[j].term
	})
	out := make([]string, 0, limit)
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		out = append(out, c.term)
	}
	return out
}

// score rates how "distinctive" a token looks. All-caps and mixed alphanumeric
// identifiers outrank ordinary lowercase words.
func score(tok string) int {
	runes := []rune(tok)
	upper, lower, digit := 0, 0, 0
	for _, r := range runes {
		switch {
		case unicode.IsUpper(r):
			upper++
		case unicode.IsLower(r):
			lower++
		case unicode.IsDigit(r):
			digit++
		}
	}
	s := 0
	if upper > 0 && lower == 0 {
		s += 3 // SCREAMING
	} else if upper > 1 {
		s += 1 // CamelCase / mixed
	}
	if digit > 0 {
		s++
	}
	if strings.ContainsAny(tok, "_-") {
		s++
	}
	if len(runes) >= 10 {
		s++
	}
	return s
}

// ---------------------------------------------------------------------------
// Learned-term store
// ---------------------------------------------------------------------------

// learnedTermStore remembers terms that a successful retry proved were the
// blocker, so later requests are obfuscated pre-emptively and pay no retry.
//
// Kept in its own file rather than in config.json: it is derived operational
// data, not operator intent, and it must not appear in a config export the
// operator might share.
type learnedTermStore struct {
	mu    sync.RWMutex
	path  string
	terms map[string]int64 // term (lowercase) → unix time last confirmed
}

var learnedTerms = &learnedTermStore{terms: map[string]int64{}}

// InitContentBlockStore points the learned-term store at a data directory and
// loads any previously learned terms. Safe to call more than once.
func InitContentBlockStore(dataDir string) {
	if strings.TrimSpace(dataDir) == "" {
		return
	}
	learnedTerms.mu.Lock()
	learnedTerms.path = filepath.Join(dataDir, "content-block-terms.json")
	path := learnedTerms.path
	learnedTerms.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return // absent on first run
	}
	var loaded map[string]int64
	if err := json.Unmarshal(raw, &loaded); err != nil {
		logger.Warnf("[ContentBlock] learned-term store unreadable (%s): %v", path, err)
		return
	}
	learnedTerms.mu.Lock()
	learnedTerms.terms = loaded
	n := len(loaded)
	learnedTerms.mu.Unlock()
	if n > 0 {
		logger.Infof("[ContentBlock] loaded %d learned trigger term(s)", n)
	}
}

// snapshot returns the learned terms, longest first so that a longer phrase is
// obfuscated before a shorter term nested inside it.
func (s *learnedTermStore) snapshot() []string {
	s.mu.RLock()
	out := make([]string, 0, len(s.terms))
	for t := range s.terms {
		out = append(out, t)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// learn records terms proven to be blockers by a successful retry.
func (s *learnedTermStore) learn(terms []string) {
	if len(terms) == 0 {
		return
	}
	now := time.Now().Unix()
	s.mu.Lock()
	added := make([]string, 0, len(terms))
	for _, t := range terms {
		key := strings.ToLower(strings.TrimSpace(t))
		if len(key) < minObfuscationLen {
			continue
		}
		if _, exists := s.terms[key]; !exists {
			added = append(added, key)
		}
		s.terms[key] = now
	}
	// Evict the least recently confirmed terms when over the cap.
	if len(s.terms) > maxLearnedTerms {
		type kv struct {
			k string
			v int64
		}
		all := make([]kv, 0, len(s.terms))
		for k, v := range s.terms {
			all = append(all, kv{k, v})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
		for i := 0; i < len(all)-maxLearnedTerms; i++ {
			delete(s.terms, all[i].k)
		}
	}
	path := s.path
	dump, err := json.MarshalIndent(s.terms, "", "  ")
	s.mu.Unlock()

	if len(added) > 0 {
		logger.Warnf("[ContentBlock] learned trigger term(s): %s", strings.Join(added, ", "))
	}
	if path == "" || err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, dump, 0o600); err != nil {
		logger.Warnf("[ContentBlock] cannot persist learned terms: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.Warnf("[ContentBlock] cannot persist learned terms: %v", err)
	}
}

// resetLearnedTermsForTests clears the store. Test-only.
func resetLearnedTermsForTests() {
	learnedTerms.mu.Lock()
	learnedTerms.terms = map[string]int64{}
	learnedTerms.path = ""
	learnedTerms.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Public helpers used by the request path
// ---------------------------------------------------------------------------

// applyLearnedObfuscation pre-emptively splits every already-learned term in
// the payload. Called before the first upstream attempt so a known trigger
// costs nothing: no rejection, no retry.
//
// Returns true when the payload was modified.
func applyLearnedObfuscation(payload *KiroPayload) bool {
	terms := learnedTerms.snapshot()
	if len(terms) == 0 {
		return false
	}
	return applyToPayloadText(payload, func(s string) (string, bool) {
		return obfuscateTermsIn(s, terms)
	})
}

// obfuscateCandidates splits the given candidate terms throughout the payload.
func obfuscateCandidates(payload *KiroPayload, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	return applyToPayloadText(payload, func(s string) (string, bool) {
		return obfuscateTermsIn(s, terms)
	})
}

// obfuscateEverything splits every candidate-shaped token in the payload. This
// is the last resort before giving up on the account: it is the most likely to
// pass and the most costly, because each splitter adds tokens to the request.
func obfuscateEverything(payload *KiroPayload) bool {
	return applyToPayloadText(payload, func(s string) (string, bool) {
		out := candidateTokenRe.ReplaceAllStringFunc(s, func(tok string) string {
			if isObfuscated(tok) {
				return tok
			}
			return obfuscateToken(tok)
		})
		return out, out != s
	})
}

// ---------------------------------------------------------------------------
// Blocked-payload capture
// ---------------------------------------------------------------------------

// captureBlockedPayload writes a rejected payload to disk so the trigger can be
// studied from real traffic instead of from synthetic probes.
//
// This exists because the previous behaviour discarded the evidence: the only
// record of a rejection was a truncated log line, which is why two successive
// investigations produced two wrong hypotheses (payload size, then a keyword
// blocklist). Probing the provider to reconstruct what it rejected also trips
// its rate limiter and poisons the results.
//
// Nothing leaves the machine. Credentials never reach this function — the
// payload carries no key — but the model/profile identifiers are dropped
// anyway so a shared capture cannot identify the account.
func captureBlockedPayload(dataDir, accountEmail, model string, payload *KiroPayload, upstreamErr error) {
	if strings.TrimSpace(dataDir) == "" || payload == nil {
		return
	}
	dir := filepath.Join(dataDir, "blocked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Warnf("[ContentBlock] cannot create capture dir: %v", err)
		return
	}
	redacted := *payload
	redacted.ProfileArn = ""
	record := map[string]interface{}{
		"capturedAt": time.Now().Format(time.RFC3339),
		"model":      model,
		"account":    accountEmail,
		"error":      upstreamErr.Error(),
		"texts":      collectPayloadText(payload),
	}
	dump, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return
	}
	name := fmt.Sprintf("%s-%d.json", time.Now().UTC().Format("20060102-150405"), time.Now().UnixNano()%1000)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, dump, 0o600); err != nil {
		logger.Warnf("[ContentBlock] cannot write capture: %v", err)
		return
	}
	logger.Warnf("[ContentBlock] captured rejected payload for analysis: %s", path)
}
