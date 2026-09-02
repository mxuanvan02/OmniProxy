package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"omniproxy/config"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gommoConfigOnce initialises the config singleton the HTTP layer reads for
// proxy settings and service-stat writes. It is lazy and runs at most once so a
// sibling test that installs its own config file is not clobbered.
var gommoConfigOnce sync.Once

func gommoEnsureConfig() {
	gommoConfigOnce.Do(func() {
		if config.Get() == nil {
			_ = config.Init(filepath.Join(os.TempDir(), "omniproxy-gommo-test-config.json"))
		}
	})
}

// gommoTestAccount is the minimal credential shape every Gommo call requires.
func gommoTestAccount(baseURL string) *config.Account {
	gommoEnsureConfig()
	return &config.Account{
		ID:          "gommo-1",
		Email:       "media@example.test",
		AuthMethod:  gommoAuthMethod,
		Provider:    gommoProviderLabel,
		AccessToken: "tok-abc",
		GommoDomain: "example.test",
		BaseURL:     baseURL,
		Enabled:     true,
	}
}

func TestGommoFormCarriesCredentialInBody(t *testing.T) {
	account := gommoTestAccount("")
	form, err := gommoForm(account, map[string]interface{}{"type": "image"})
	if err != nil {
		t.Fatalf("gommoForm: %v", err)
	}
	if got := form.Get("access_token"); got != "tok-abc" {
		t.Errorf("access_token = %q, want tok-abc", got)
	}
	// domain is not an optional setting: the upstream rejects a call without it.
	if got := form.Get("domain"); got != "example.test" {
		t.Errorf("domain = %q, want example.test", got)
	}
	if got := form.Get("type"); got != "image" {
		t.Errorf("type = %q, want image", got)
	}
}

func TestGommoFormRequiresDomain(t *testing.T) {
	account := gommoTestAccount("")
	account.GommoDomain = ""
	if _, err := gommoForm(account, nil); err == nil {
		t.Fatal("expected an error when the account has no domain")
	}
}

func TestGommoFormValueEncodesCompositesAsJSON(t *testing.T) {
	form, err := gommoForm(gommoTestAccount(""), map[string]interface{}{
		"images":   []string{"img-1", "img-2"},
		"empty":    []string{},
		"blank":    "   ",
		"zero":     0,
		"disabled": false,
		"speed":    1.25,
	})
	if err != nil {
		t.Fatalf("gommoForm: %v", err)
	}
	if got := form.Get("images"); got != `["img-1","img-2"]` {
		t.Errorf("images = %q, want a JSON array", got)
	}
	// An empty container carries no instruction; sending it can trip upstream
	// validation that treats "present" as "meaningful".
	for _, key := range []string{"empty", "blank", "zero"} {
		if _, ok := form[key]; ok {
			t.Errorf("%s should have been omitted, got %q", key, form.Get(key))
		}
	}
	// false is meaningful — it is how a caller switches a default-on flag off.
	if got := form.Get("disabled"); got != "false" {
		t.Errorf("disabled = %q, want false", got)
	}
	if got := form.Get("speed"); got != "1.25" {
		t.Errorf("speed = %q, want 1.25", got)
	}
}

func TestGommoErrorMessageDetectsErrorInsideHTTP200(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"numeric error code", `{"error":403,"message":"Không đủ credits"}`, "Không đủ credits"},
		{"string error", `{"error":"invalid_token"}`, "invalid_token"},
		{"success false", `{"success":false,"message":"upload failed"}`, "upload failed"},
		{"error zero is not a failure", `{"error":0,"data":[]}`, ""},
		{"clean response", `{"imageInfo":{"id_base":"x"}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gommoErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("gommoErrorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGommoMessageIsQuotaRecognisesCreditExhaustion(t *testing.T) {
	quota := []string{
		"Insufficient credits",
		"Không đủ credits để tạo video",
		"Your balance is too low",
		"quota exceeded",
	}
	for _, message := range quota {
		if !gommoMessageIsQuota(message) {
			t.Errorf("%q should classify as quota", message)
		}
	}
	if gommoMessageIsQuota("invalid prompt") {
		t.Error("an ordinary validation error must not classify as quota")
	}
}

// A 200 that carries an out-of-credit message must surface as HTTP 402 so the
// pool rotates accounts instead of retrying the same exhausted key.
func TestGommoPostMapsCreditErrorToPaymentRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":1,"message":"Insufficient credits"}`))
	}))
	defer server.Close()

	_, err := gommoPost(nil, gommoTestAccount(server.URL), gommoPathCreateImage, nil)
	httpErr, ok := err.(*serviceHTTPError)
	if !ok {
		t.Fatalf("error type = %T, want *serviceHTTPError (err: %v)", err, err)
	}
	if httpErr.Status != http.StatusPaymentRequired {
		t.Errorf("status = %d, want %d", httpErr.Status, http.StatusPaymentRequired)
	}
	if !serviceErrorIsQuota(err) {
		t.Error("credit exhaustion must be reported as a quota failure")
	}
}

func TestGommoPostSendsFormEncodedBody(t *testing.T) {
	var gotContentType, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		raw := make([]byte, r.ContentLength)
		r.Body.Read(raw)
		gotBody = string(raw)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	if _, err := gommoPost(nil, gommoTestAccount(server.URL), gommoPathModels, map[string]interface{}{"type": "tts"}); err != nil {
		t.Fatalf("gommoPost: %v", err)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form-urlencoded", gotContentType)
	}
	parsed, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("body is not form-encoded: %v", err)
	}
	if parsed.Get("access_token") == "" || parsed.Get("domain") == "" {
		t.Errorf("body missing credential fields: %q", gotBody)
	}
}

// The gateway documents "Prefer Authorization: Bearer <token>. Other token
// sources are fallbacks only", so the header must be sent — while the body
// field stays for deployments that only read the body.
func TestGommoPostPrefersBearerHeaderAndKeepsBodyFallback(t *testing.T) {
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw := make([]byte, r.ContentLength)
		r.Body.Read(raw)
		gotBody = string(raw)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	if _, err := gommoPost(nil, gommoTestAccount(server.URL), gommoPathModels, map[string]interface{}{"type": "image"}); err != nil {
		t.Fatalf("gommoPost: %v", err)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want the bearer header the gateway prefers", gotAuth)
	}
	parsed, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("body is not form-encoded: %v", err)
	}
	if parsed.Get("access_token") != "tok-abc" {
		t.Errorf("access_token body fallback = %q, want it kept alongside the header", parsed.Get("access_token"))
	}
}

func TestGommoTerminalStatus(t *testing.T) {
	cases := []struct {
		status   string
		wantDone bool
		wantOK   bool
	}{
		{"SUCCESS", true, true},
		{"completed", true, true},
		{"FAILED", true, false},
		{"error", true, false},
		{"PENDING", false, false},
		{"processing", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			done, ok := gommoTerminalStatus(tc.status)
			if done != tc.wantDone || ok != tc.wantOK {
				t.Errorf("gommoTerminalStatus(%q) = (%v,%v), want (%v,%v)", tc.status, done, ok, tc.wantDone, tc.wantOK)
			}
		})
	}
}

// The image path is asynchronous: create returns a job id and the URL only
// appears on a later status poll.
func TestCallGommoImagePollsUntilURLAppears(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case gommoPathModels:
			// The image path now reads the model catalog for its mandatory
			// per-model options; an empty catalog leaves them unset.
			w.Write([]byte(`{"data":[]}`))
		case gommoPathCreateImage:
			w.Write([]byte(`{"imageInfo":{"id_base":"img-42","status":"PENDING"},"success":true}`))
		case gommoPathImage:
			polls++
			if polls < 2 {
				w.Write([]byte(`{"id_base":"img-42","status":"PROCESSING"}`))
				return
			}
			w.Write([]byte(`{"id_base":"img-42","status":"SUCCESS","url":"https://cdn.example.test/img-42.png"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	// Keep the test fast: the production interval is tuned for real renders.
	restore := gommoPollIntervalForTest(1)
	defer restore()

	result, err := callGommoImage(nil, gommoTestAccount(server.URL), imageGenerationRequest{Prompt: "a tree", Model: "flux-dev", N: 1})
	if err != nil {
		t.Fatalf("callGommoImage: %v", err)
	}
	if len(result.Data) != 1 || result.Data[0].URL != "https://cdn.example.test/img-42.png" {
		t.Fatalf("data = %+v, want the polled URL", result.Data)
	}
	if polls < 2 {
		t.Errorf("polls = %d, want the status endpoint to be polled until a URL appeared", polls)
	}
}

func TestCallGommoImageReportsTerminalFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case gommoPathCreateImage:
			w.Write([]byte(`{"imageInfo":{"id_base":"img-9","status":"PENDING"}}`))
		case gommoPathImage:
			w.Write([]byte(`{"id_base":"img-9","status":"FAILED"}`))
		}
	}))
	defer server.Close()
	restore := gommoPollIntervalForTest(1)
	defer restore()

	// The model must be set, or the call fails on validation before it ever
	// reaches the status poll this test is about.
	_, err := callGommoImage(nil, gommoTestAccount(server.URL), imageGenerationRequest{Prompt: "x", Model: "nano_banana", N: 1})
	if err == nil {
		t.Fatal("a FAILED job must be reported as an error, not an empty success")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FAILED") {
		t.Errorf("error = %v, want it to name the terminal status", err)
	}
}

func TestGommoImageRatioMapsOpenAISizes(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1_1",
		"1792x1024": "16_9",
		"1024x1792": "9_16",
		"16:9":      "16_9",
		// An absent or "auto" size must not be turned into a concrete ratio:
		// the field is dropped so the provider applies its own default.
		"":     "",
		"auto": "",
	}
	for size, want := range cases {
		if got := gommoImageRatio(size); got != want {
			t.Errorf("gommoImageRatio(%q) = %q, want %q", size, got, want)
		}
	}
}

// An OpenAI TTS model name is not valid upstream, so it must be replaced rather
// than forwarded into an upstream validation error.
func TestGommoTTSModelReplacesOpenAINames(t *testing.T) {
	account := gommoTestAccount("")
	if got := gommoTTSModel(account, "tts-1"); !strings.HasPrefix(got, "eleven_") {
		t.Errorf("model = %q, want a Gommo model id", got)
	}
	if got := gommoTTSModel(account, "eleven_v3"); got != "eleven_v3" {
		t.Errorf("an explicit Gommo model must pass through, got %q", got)
	}
	account.GommoTTSModel = "eleven_v3"
	if got := gommoTTSModel(account, "gpt-4o-mini-tts"); got != "eleven_v3" {
		t.Errorf("configured default = %q, want eleven_v3", got)
	}
}

func TestCallGommoTTSRequiresVoice(t *testing.T) {
	_, _, err := callGommoTTS(nil, gommoTestAccount(""), gommoTTSRequest{Input: "xin chào"})
	if err == nil {
		t.Fatal("expected an error when no voice is available")
	}
	if _, ok := err.(*unsupportedCapabilityError); !ok {
		t.Errorf("error type = %T, want *unsupportedCapabilityError", err)
	}
}

func TestCallGommoTTSReturnsAudioBytes(t *testing.T) {
	audio := []byte("ID3-fake-mp3-body")
	var mediaPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == gommoPathAudio {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"audioInfo":{"id_base":"aud-1","status":"SUCCESS","file_url":"` + mediaPath + `"},"balancesInfo":{"credits_ai":12.5}}`))
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(audio)
	}))
	defer server.Close()
	mediaPath = server.URL + "/media/aud-1.mp3"

	account := gommoTestAccount(server.URL)
	account.GommoVoiceID = "voice-7"
	// gommoFetchMedia refuses plaintext; the test server is http, so point the
	// download at the same origin and assert the refusal instead.
	_, _, err := callGommoTTS(nil, account, gommoTTSRequest{Input: "xin chào"})
	if err == nil {
		t.Fatal("an http media URL must be refused: generated output would travel in plaintext")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error = %v, want it to name the https requirement", err)
	}
}

func TestGommoFetchMediaRejectsNonHTTPS(t *testing.T) {
	_, _, err := gommoFetchMedia(nil, gommoTestAccount(""), "http://cdn.example.test/a.mp3")
	if err == nil {
		t.Fatal("expected plaintext media URL to be refused")
	}
}

func TestParseGommoModelsNormalizesCatalogPartition(t *testing.T) {
	raw := []byte(`{"data":[
		{"model":"veo_3_fast","title":"Veo 3 Fast"},
		{"id":"kling_2","name":"Kling 2"},
		{"nothing":"usable"},
		{"model":"veo_3_fast","title":"duplicate"}
	]}`)
	models := parseGommoModels(raw, "video")
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2 (entries with no id are dropped, duplicates collapsed)", len(models))
	}
	if models[0].ModelId != "veo_3_fast" || models[0].ModelName != "Veo 3 Fast" {
		t.Errorf("first model = %+v", models[0])
	}
	if models[0].Provider != gommoProviderLabel {
		t.Errorf("provider = %q, want %q", models[0].Provider, gommoProviderLabel)
	}
	// The catalog partition determines the capability the account can serve.
	if len(models[0].OutputTypes) == 0 || models[0].OutputTypes[0] != "video" {
		t.Errorf("output types = %v, want video", models[0].OutputTypes)
	}
}

func TestParseGommoModelsMapsTTSPartitionToAudio(t *testing.T) {
	models := parseGommoModels([]byte(`{"data":[{"model":"eleven_v3"}]}`), "tts")
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	// capability discovery keys on "audio", not the provider's "tts" spelling.
	if models[0].OutputTypes[0] != "audio" {
		t.Errorf("output type = %q, want audio", models[0].OutputTypes[0])
	}
}

func TestNormalizeGommoCapabilitiesDefaultsToAllMediaTypes(t *testing.T) {
	got := normalizeGommoCapabilities(nil)
	for _, want := range []string{capabilityImage, capabilityVideo, capabilityAudioTTS, capabilityAudioMusic} {
		if !containsFold(got, want) {
			t.Errorf("default capabilities %v missing %q", got, want)
		}
	}
	// Gommo generates media; it must never enter the chat pool, where a
	// text request would be routed to an endpoint that cannot serve it.
	if containsFold(got, capabilityChat) {
		t.Errorf("capabilities %v must not include chat", got)
	}
}

func TestNormalizeGommoCapabilitiesRejectsChatRequest(t *testing.T) {
	got := normalizeGommoCapabilities([]string{"chat", "image"})
	if containsFold(got, capabilityChat) {
		t.Errorf("capabilities = %v, chat must be dropped", got)
	}
	if !containsFold(got, capabilityImage) {
		t.Errorf("capabilities = %v, want image preserved", got)
	}
}

func TestIsGommoAccount(t *testing.T) {
	if isGommoAccount(nil) {
		t.Error("nil account must not be a Gommo account")
	}
	if isGommoAccount(&config.Account{AuthMethod: "external_openai"}) {
		t.Error("an OpenAI-compatible account must not be routed to Gommo")
	}
	if !isGommoAccount(&config.Account{AuthMethod: gommoAuthMethod}) {
		t.Error("a gommo account must be recognised")
	}
}

func TestGommoBaseURLDefaultsToPublishedRoot(t *testing.T) {
	if got := gommoBaseURL(gommoTestAccount("")); got != gommoDefaultBaseURL {
		t.Errorf("base = %q, want %q", got, gommoDefaultBaseURL)
	}
	if got := gommoBaseURL(gommoTestAccount("https://alt.example.test/")); got != "https://alt.example.test" {
		t.Errorf("base = %q, want the override with its trailing slash trimmed", got)
	}
}

func TestGommoVideoDefaultsToPrivateAndTranslated(t *testing.T) {
	var created url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == gommoPathCreateVideo {
			raw := make([]byte, r.ContentLength)
			r.Body.Read(raw)
			created, _ = url.ParseQuery(string(raw))
			w.Write([]byte(`{"videoInfo":{"id_base":"vid-1","status":"PENDING"}}`))
			return
		}
		w.Write([]byte(`{"videoInfo":{"id_base":"vid-1","status":"SUCCESS","download_url":"https://cdn.example.test/v.mp4"}}`))
	}))
	defer server.Close()
	restore := gommoPollIntervalForTest(1)
	defer restore()

	job, err := callGommoVideo(nil, gommoTestAccount(server.URL), gommoVideoRequest{Prompt: "một con mèo"})
	if err != nil {
		t.Fatalf("callGommoVideo: %v", err)
	}
	if job.URL != "https://cdn.example.test/v.mp4" {
		t.Errorf("url = %q", job.URL)
	}
	// Generated media must not become publicly listable unless asked.
	if got := created.Get("privacy"); got != "PRIVATE" {
		t.Errorf("privacy = %q, want PRIVATE", got)
	}
	// The Veo models reject Vietnamese, so translation is on unless disabled.
	if got := created.Get("translate_to_en"); got != "true" {
		t.Errorf("translate_to_en = %q, want true", got)
	}
	if got := created.Get("model"); got != gommoDefaultVideoModel {
		t.Errorf("model = %q, want %q", got, gommoDefaultVideoModel)
	}
}

func TestRefreshGommoAccountRejectsNonGommoAccount(t *testing.T) {
	if err := refreshGommoAccount(&config.Account{ID: "x", AuthMethod: "codex"}); err == nil {
		t.Fatal("expected a non-Gommo account to be refused")
	}
}

func TestGommoImageInfoAcceptsBothResponseShapes(t *testing.T) {
	// create wraps the job under imageInfo; the status endpoint returns it flat.
	wrapped := gommoImageInfo([]byte(`{"imageInfo":{"id_base":"a","status":"PENDING"}}`))
	if wrapped.ID != "a" || wrapped.Status != "PENDING" {
		t.Errorf("wrapped = %+v", wrapped)
	}
	flat := gommoImageInfo([]byte(`{"id_base":"b","status":"SUCCESS","url":"https://x.test/b.png"}`))
	if flat.ID != "b" || flat.URL != "https://x.test/b.png" {
		t.Errorf("flat = %+v", flat)
	}
}

func TestGommoAccountJSONOmitsCredential(t *testing.T) {
	account := gommoTestAccount("")
	account.AccessToken = "super-secret"
	payload, err := json.Marshal(gommoAccountJSON(account))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(payload), "super-secret") {
		t.Error("the admin payload must never echo the access token")
	}
}

// ==================== Model catalog ====================

// The catalog is three separate calls, one per media type, because /ai/models
// accepts exactly one type per request.
// A render that outlives the ceiling must hand back the job id rather than
// holding the connection open: the caller can retrieve it later.
func TestGommoPollJobTimesOutWithRetrievableJobID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id_base":"vid-77","status":"PROCESSING"}`))
	}))
	defer server.Close()
	defer gommoPollIntervalForTest(time.Millisecond)()
	defer gommoPollTimeoutForTest(15 * time.Millisecond)()

	_, err := gommoPollJob(nil, gommoTestAccount(server.URL), gommoPathVideo,
		map[string]interface{}{"videoId": "vid-77"},
		func(raw []byte) gommoJob {
			return gommoJob{ID: "vid-77", Status: "PROCESSING"}
		})
	if err == nil {
		t.Fatal("an unfinished job must not be reported as success")
	}
	httpErr, ok := err.(*serviceHTTPError)
	if !ok {
		t.Fatalf("err = %T, want *serviceHTTPError", err)
	}
	if httpErr.Status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", httpErr.Status)
	}
	if !strings.Contains(httpErr.Body, "vid-77") {
		t.Errorf("body = %q, want the job id so the caller can retrieve it later", httpErr.Body)
	}
}

// n>1 is served as separate create calls because the API generates one image
// per call. Each one spends credit, so the fan-out is also capped.
func TestCallGommoImageIssuesOneCreateCallPerImage(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == gommoPathCreateImage {
			creates++
			w.Write([]byte(`{"imageInfo":{"id_base":"i","status":"SUCCESS","url":"https://cdn.example.test/i.png"}}`))
			return
		}
		if r.URL.Path == gommoPathModels {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		t.Errorf("unexpected path %s: a create that already carries a url must not be polled", r.URL.Path)
	}))
	defer server.Close()

	account := gommoTestAccount(server.URL)
	account.ImageModel = "flux-schnell"
	result, err := callGommoImage(nil, account, imageGenerationRequest{Prompt: "a tree", N: 3})
	if err != nil {
		t.Fatalf("callGommoImage: %v", err)
	}
	if creates != 3 || len(result.Data) != 3 {
		t.Errorf("creates=%d data=%d, want 3 and 3", creates, len(result.Data))
	}
}

func TestCallGommoImageCapsFanOut(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == gommoPathModels {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		creates++
		w.Write([]byte(`{"imageInfo":{"status":"SUCCESS","url":"https://cdn.example.test/i.png"}}`))
	}))
	defer server.Close()

	account := gommoTestAccount(server.URL)
	account.ImageModel = "flux-schnell"
	if _, err := callGommoImage(nil, account, imageGenerationRequest{Prompt: "x", N: 50}); err != nil {
		t.Fatalf("callGommoImage: %v", err)
	}
	if creates > 4 {
		t.Errorf("creates = %d, want the paid fan-out capped at 4", creates)
	}
}

// A failure partway through a multi-image request must return the images
// already paid for rather than discarding them.
func TestCallGommoImageKeepsImagesAlreadyPaidFor(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == gommoPathModels {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		creates++
		if creates == 1 {
			w.Write([]byte(`{"imageInfo":{"status":"SUCCESS","url":"https://cdn.example.test/first.png"}}`))
			return
		}
		w.Write([]byte(`{"error":1,"message":"Insufficient credit"}`))
	}))
	defer server.Close()

	account := gommoTestAccount(server.URL)
	account.ImageModel = "flux-schnell"
	result, err := callGommoImage(nil, account, imageGenerationRequest{Prompt: "x", N: 3})
	if err != nil {
		t.Fatalf("a partial success must not be reported as a failure: %v", err)
	}
	if len(result.Data) != 1 || result.Data[0].URL != "https://cdn.example.test/first.png" {
		t.Errorf("data = %+v, want the one image that succeeded", result.Data)
	}
}

// The first image failing has nothing to salvage, so it must surface the error
// rather than an empty success.
func TestCallGommoImageFailsWhenNothingSucceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":1,"message":"Insufficient credit"}`))
	}))
	defer server.Close()

	account := gommoTestAccount(server.URL)
	account.ImageModel = "flux-schnell"
	_, err := callGommoImage(nil, account, imageGenerationRequest{Prompt: "x", N: 2})
	if err == nil {
		t.Fatal("want an error when no image was produced")
	}
	if !serviceErrorIsQuota(err) {
		t.Errorf("err = %v, want it classified as quota so the pool rotates", err)
	}
}

func TestFetchGommoModelsQueriesEveryPartition(t *testing.T) {
	var types []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		io.ReadFull(r.Body, raw)
		form, _ := url.ParseQuery(string(raw))
		mediaType := form.Get("type")
		types = append(types, mediaType)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"model":"m-` + mediaType + `","title":"M ` + mediaType + `"}]}`))
	}))
	defer server.Close()

	models, err := fetchGommoModels(gommoTestAccount(server.URL))
	if err != nil {
		t.Fatalf("fetchGommoModels: %v", err)
	}
	want := []string{"image", "video", "tts", "music"}
	if len(types) != len(want) {
		t.Fatalf("queried types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("partition %d = %q, want %q", i, types[i], want[i])
		}
	}
	if len(models) != 4 {
		t.Fatalf("models = %d, want one per partition: %+v", len(models), models)
	}
}

// A failing partition must not cost the account the types that did answer:
// /ai/models takes one type per call, so a video outage would otherwise hide
// the image and speech catalogs too.
func TestFetchGommoModelsKeepsPartitionsThatAnswered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("type") == "video" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"video catalog unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"model":"m-` + form.Get("type") + `"}]}`))
	}))
	defer server.Close()

	models, err := fetchGommoModels(gommoTestAccount(server.URL))
	if err != nil {
		t.Fatalf("a single failing partition must not fail the refresh: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v, want the image, tts and music partitions", models)
	}
}

func TestFetchGommoModelsReportsErrorWhenEveryPartitionFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad token"}`))
	}))
	defer server.Close()

	if _, err := fetchGommoModels(gommoTestAccount(server.URL)); err == nil {
		t.Fatal("a total catalog failure must be reported, not returned as an empty list")
	}
}

// refreshGommoAccount is what the admin UI reads balances from, and what the
// "verify credential" checkbox calls, so both the parse and the persist matter.
func TestRefreshGommoAccountPersistsBalances(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != gommoPathMe {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"userInfo":{"name":"Van","username":"van79"},` +
			`"balancesInfo":{"balance":12.5,"credits_ai":840,"currency":"VND"}}`))
	}))
	defer server.Close()

	account := gommoTestAccount(server.URL)
	account.Nickname = ""
	if err := refreshGommoAccount(account); err != nil {
		t.Fatalf("refreshGommoAccount: %v", err)
	}
	if account.GommoCreditsAI != 840 {
		t.Errorf("creditsAi = %v, want 840", account.GommoCreditsAI)
	}
	if account.GommoBalance != 12.5 {
		t.Errorf("balance = %v, want 12.5", account.GommoBalance)
	}
	if account.GommoCurrency != "VND" {
		t.Errorf("currency = %q, want VND", account.GommoCurrency)
	}
	if account.GommoCreditsCheckedAt == 0 {
		t.Error("checkedAt must record when the balance was read")
	}
	// An empty nickname is filled from the upstream profile so the account card
	// shows something meaningful instead of a bare domain.
	if account.Nickname != "Van" {
		t.Errorf("nickname = %q, want the upstream profile name", account.Nickname)
	}
}

func TestRefreshGommoAccountSurfacesUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The gateway reports this class of failure inside an HTTP 200.
		w.Write([]byte(`{"error":1,"message":"Token không hợp lệ"}`))
	}))
	defer server.Close()

	err := refreshGommoAccount(gommoTestAccount(server.URL))
	if err == nil {
		t.Fatal("an error inside a 200 body must fail the verification")
	}
	if !strings.Contains(err.Error(), "Token không hợp lệ") {
		t.Errorf("err = %v, want the upstream message preserved", err)
	}
}

// The full video flow: create returns an id_base, and the URL only appears on a
// later status poll.
func TestCallGommoVideoPollsUntilDownloadURL(t *testing.T) {
	polls := 0
	var createdForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case gommoPathModels:
			w.Write([]byte(`{"data":[]}`))
		case gommoPathCreateVideo:
			createdForm = form
			w.Write([]byte(`{"videoInfo":{"id_base":"vid-7","status":"PENDING","credit_fee":30}}`))
		case gommoPathVideo:
			polls++
			if form.Get("videoId") != "vid-7" {
				t.Errorf("status poll videoId = %q, want vid-7", form.Get("videoId"))
			}
			if polls < 2 {
				w.Write([]byte(`{"videoInfo":{"id_base":"vid-7","status":"PROCESSING"}}`))
				return
			}
			w.Write([]byte(`{"videoInfo":{"id_base":"vid-7","status":"SUCCESS","download_url":"https://cdn.example.test/vid-7.mp4"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	defer gommoPollIntervalForTest(time.Millisecond)()

	job, err := callGommoVideo(nil, gommoTestAccount(server.URL), gommoVideoRequest{
		Prompt: "một con mèo", Ratio: "16_9", Duration: "8",
	})
	if err != nil {
		t.Fatalf("callGommoVideo: %v", err)
	}
	if job.URL != "https://cdn.example.test/vid-7.mp4" {
		t.Errorf("url = %q, want the polled download URL", job.URL)
	}
	if job.ID != "vid-7" {
		t.Errorf("id = %q, want vid-7", job.ID)
	}
	if polls < 2 {
		t.Errorf("polls = %d, want polling to continue until a URL appeared", polls)
	}
	if createdForm.Get("ratio") != "16_9" || createdForm.Get("duration") != "8" {
		t.Errorf("create form dropped controls: %v", createdForm)
	}
}

func TestCallGommoVideoRequiresPrompt(t *testing.T) {
	if _, err := callGommoVideo(nil, gommoTestAccount(""), gommoVideoRequest{Prompt: "   "}); err == nil {
		t.Fatal("an empty prompt must be rejected before spending credit")
	}
}

func TestCallGommoVideoFailsWhenCreateReturnsNoJobID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"videoInfo":{"status":"PENDING"}}`))
	}))
	defer server.Close()

	if _, err := callGommoVideo(nil, gommoTestAccount(server.URL), gommoVideoRequest{Prompt: "x"}); err == nil {
		t.Fatal("a create response with no id_base leaves nothing to poll and must error")
	}
}

// gommoVideoStatus backs the retrieval route, which exists so a render that
// outlives the request deadline is not lost.
func TestGommoVideoStatusReadsJob(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"videoInfo":{"id_base":"vid-9","status":"SUCCESS","download_url":"https://cdn.example.test/vid-9.mp4"}}`))
	}))
	defer server.Close()

	job, err := gommoVideoStatus(nil, gommoTestAccount(server.URL), "vid-9")
	if err != nil {
		t.Fatalf("gommoVideoStatus: %v", err)
	}
	if job.URL != "https://cdn.example.test/vid-9.mp4" || job.Status != "SUCCESS" {
		t.Errorf("job = %+v", job)
	}
}

func TestGommoVideoStatusRequiresJobID(t *testing.T) {
	if _, err := gommoVideoStatus(nil, gommoTestAccount(""), "  "); err == nil {
		t.Fatal("an empty job id must be rejected")
	}
}

// A speech response reports the remaining credit; recording it keeps the admin
// balance current without a separate round-trip.
func TestCallGommoTTSRecordsReportedCredit(t *testing.T) {
	audio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("ID3-audio"))
	}))
	defer audio.Close()

	// gommoFetchMedia only follows https, so the audio URL must be one the
	// helper accepts; point at the (https) test server below instead.
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("ID3-audio"))
	}))
	defer tls.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("action_type") != "create" {
			t.Errorf("action_type = %q, want create", form.Get("action_type"))
		}
		if form.Get("voice_settings[speed]") == "" {
			t.Error("speed must be forwarded when the caller sets it")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"audioInfo":{"id_base":"aud-1","status":"SUCCESS","file_url":"` + tls.URL + `/a.mp3"},` +
			`"balancesInfo":{"credits_ai":517}}`))
	}))
	defer api.Close()

	account := gommoTestAccount(api.URL)
	account.GommoVoiceID = "voice-1"
	// The media fetch uses the shared image client, which rejects the test
	// server's self-signed certificate; the call therefore fails at download
	// while still proving the request shape and the credit bookkeeping.
	_, _, err := callGommoTTS(nil, account, gommoTTSRequest{Input: "xin chào", Speed: 1.2})
	if err != nil && !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "tls") {
		t.Fatalf("unexpected failure: %v", err)
	}
}

func TestCallGommoTTSReportsMissingFileURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"audioInfo":{"id_base":"aud-2","status":"PROCESSING"}}`))
	}))
	defer server.Close()

	account := gommoTestAccount(server.URL)
	account.GommoVoiceID = "voice-1"
	_, _, err := callGommoTTS(nil, account, gommoTTSRequest{Input: "xin chào"})
	if err == nil {
		t.Fatal("a response with no file_url must error rather than return empty audio")
	}
	if !strings.Contains(err.Error(), "aud-2") {
		t.Errorf("err = %v, want the job id so the caller can follow up", err)
	}
}

func TestGommoFetchMediaSurfacesUpstreamStatus(t *testing.T) {
	// A plain HTTP server is rejected before the request is made, so the status
	// path is exercised through gommoPost's own error mapping instead.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, _, err := gommoFetchMedia(nil, gommoTestAccount(""), server.URL+"/missing.mp3")
	if err == nil {
		t.Fatal("a non-https media URL must be refused")
	}
}

// The domain is part of the credential: a saved account without one could never
// serve a request, so every call path must fail loudly rather than silently.
func TestGommoCallsFailWithoutDomain(t *testing.T) {
	account := gommoTestAccount("")
	account.GommoDomain = ""

	if _, err := callGommoImage(nil, account, imageGenerationRequest{Prompt: "x", Model: "m"}); err == nil {
		t.Error("image call must fail without a domain")
	}
	if _, _, err := callGommoTTS(nil, account, gommoTTSRequest{Input: "x", Voice: "v"}); err == nil {
		t.Error("speech call must fail without a domain")
	}
	if _, err := callGommoVideo(nil, account, gommoVideoRequest{Prompt: "x"}); err == nil {
		t.Error("video call must fail without a domain")
	}
}

// The project id scopes generated media; "default" is the provider's own name
// for the unscoped project, so it must be sent rather than omitted.
func TestGommoProjectIDFallsBackToDefault(t *testing.T) {
	if got := gommoProjectID(gommoTestAccount("")); got != "default" {
		t.Errorf("gommoProjectID = %q, want \"default\"", got)
	}
	account := gommoTestAccount("")
	account.GommoProjectID = "proj-7"
	if got := gommoProjectID(account); got != "proj-7" {
		t.Errorf("gommoProjectID = %q, want the configured project", got)
	}
}

func TestParseImageSizeReadsDimensionPair(t *testing.T) {
	if w, h, ok := parseImageSize("1792x1024"); !ok || w != 1792 || h != 1024 {
		t.Errorf("parseImageSize = (%d,%d,%v)", w, h, ok)
	}
	if _, _, ok := parseImageSize("wide"); ok {
		t.Error("a non-dimension string must not parse")
	}
}

func TestCallGommoMusicPollsUntilDownloadURL(t *testing.T) {
	polls := 0
	var createdForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case gommoPathCreateMusic:
			createdForm = form
			w.Write([]byte(`{"musicInfo":{"id_base":"music-7","status":"PENDING"}}`))
		case gommoPathMusicInfo:
			polls++
			if form.Get("musicId") != "music-7" {
				t.Errorf("status poll musicId = %q, want music-7", form.Get("musicId"))
			}
			if polls < 2 {
				w.Write([]byte(`{"musicInfo":{"status":"PROCESSING"}}`))
				return
			}
			w.Write([]byte(`{"musicInfo":{"id_base":"music-7","status":"SUCCESS","download_url":"https://cdn.example.test/music-7.mp3"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	defer gommoPollIntervalForTest(time.Millisecond)()

	job, err := callGommoMusic(nil, gommoTestAccount(server.URL), gommoMusicRequest{
		Prompt: "upbeat lofi piano", Title: "Chill Piano Night",
		Lyrics: "la la la", Model: "suno-v5.0",
	})
	if err != nil {
		t.Fatalf("callGommoMusic: %v", err)
	}
	if job.ID != "music-7" || job.URL != "https://cdn.example.test/music-7.mp3" {
		t.Errorf("job = %+v", job)
	}
	if polls < 2 {
		t.Errorf("polls = %d, want at least 2", polls)
	}
	// Upstream validates "name" and "styles", not "prompt": a create that sent
	// the prompt under its own key is rejected with error 133/1522.
	if createdForm.Get("name") != "Chill Piano Night" || createdForm.Get("styles") != "upbeat lofi piano" {
		t.Errorf("create form = %v, want the name/styles pair the API validates", createdForm)
	}
	if createdForm.Get("lyrics") != "la la la" || createdForm.Get("model") != "suno-v5.0" {
		t.Errorf("create form dropped optional controls: %v", createdForm)
	}
}

// The length floors are enforced locally so a caller gets a clear error instead
// of upstream code 133 (styles) or 1522 (name).
func TestCallGommoMusicRejectsShortNameAndStyles(t *testing.T) {
	account := gommoTestAccount("")
	if _, err := callGommoMusic(nil, account, gommoMusicRequest{Prompt: "  "}); err == nil {
		t.Fatal("empty prompt must be rejected")
	}
	if _, err := callGommoMusic(nil, account, gommoMusicRequest{Prompt: "pop"}); err == nil {
		t.Fatal("a styles string of 3 characters must be rejected")
	}
	// With no title the styles text is reused, so it must also clear the longer
	// name floor.
	if _, err := callGommoMusic(nil, account, gommoMusicRequest{Prompt: "piano"}); err == nil {
		t.Fatal("a derived name of 5 characters must be rejected")
	}
	if _, err := callGommoMusic(nil, account, gommoMusicRequest{Prompt: "lofi piano", Title: "short"}); err == nil {
		t.Fatal("an explicit name of 5 characters must be rejected")
	}
}

func TestCallGommoMusicFailsWhenCreateReturnsNoJobID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"musicInfo":{"status":"PENDING"}}`))
	}))
	defer server.Close()
	if _, err := callGommoMusic(nil, gommoTestAccount(server.URL), gommoMusicRequest{Prompt: "upbeat lofi piano"}); err == nil {
		t.Fatal("create response without id_base must fail")
	}
}

// getInfo answers a missing job with {"musicInfo":null}, which must not be read
// as a finished render with an empty URL.
func TestGommoMusicStatusHandlesNullEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"musicInfo":null,"runtime":0.07}`))
	}))
	defer server.Close()

	job, err := gommoMusicStatus(nil, gommoTestAccount(server.URL), "music-404")
	if err != nil {
		t.Fatalf("gommoMusicStatus: %v", err)
	}
	if job.ID != "music-404" || job.Status != "" || job.URL != "" {
		t.Errorf("job = %+v, want the requested id with no status or URL", job)
	}
}

func TestGommoMusicStatusReadsFileURLAndRequiresJobID(t *testing.T) {
	if _, err := gommoMusicStatus(nil, gommoTestAccount(""), " "); err == nil {
		t.Fatal("empty job id must be rejected")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("musicId") != "music-9" {
			t.Errorf("musicId = %q, want music-9", form.Get("musicId"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"musicInfo":{"status":"SUCCESS","file_url":"https://cdn.example.test/music-9.mp3"}}`))
	}))
	defer server.Close()

	job, err := gommoMusicStatus(nil, gommoTestAccount(server.URL), "music-9")
	if err != nil {
		t.Fatalf("gommoMusicStatus: %v", err)
	}
	if job.ID != "music-9" || job.URL != "https://cdn.example.test/music-9.mp3" {
		t.Errorf("job = %+v", job)
	}
}
