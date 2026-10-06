package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"omniproxy/config"
)

// These tests exercise the update endpoints without ever running a real update:
// every case either stops before the spawn or points OMNIPROXY at a temp tree
// holding a stub script that only echoes. Nothing here touches the network, the
// release API, or the installed binary.

// chdirToTempUpdateTree moves the test into a scratch directory laid out the way
// the endpoints expect (scripts/ beside data/), so the disk state they read and
// write lands there instead of in the repo.
func chdirToTempUpdateTree(t *testing.T, scriptBody string) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if scriptBody != "" {
		if err := os.MkdirAll("scripts", 0o755); err != nil {
			t.Fatalf("mkdir scripts: %v", err)
		}
		if err := os.WriteFile(filepath.Join("scripts", "update.sh"), []byte(scriptBody), 0o755); err != nil {
			t.Fatalf("write stub script: %v", err)
		}
	}
	return dir
}

func waitForUpdateSettled(t *testing.T, h *Handler) updateState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := readUpdateState(); !st.Running && st.ExitCode != nil {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("update never settled; state=%+v log=%q", readUpdateState(), readUpdateLog())
	return updateState{}
}

func TestUpdateStatusWithNoStateReportsIdle(t *testing.T) {
	chdirToTempUpdateTree(t, "")
	h := &Handler{}

	rec := httptest.NewRecorder()
	h.apiGetUpdateStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/api/update/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Running  bool   `json:"running"`
		ExitCode *int   `json:"exitCode"`
		Log      string `json:"log"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Running {
		t.Error("running = true with no state file, want false")
	}
	if body.ExitCode != nil {
		t.Errorf("exitCode = %v, want null", *body.ExitCode)
	}
	if body.Log != "" {
		t.Errorf("log = %q, want empty", body.Log)
	}
}

// The one-click button is a POST; anything else must not start a run.
func TestUpdateStartRequiresAdminToken(t *testing.T) {
	initConfigForTests(t)
	if err := config.SetPassword("correct-horse-battery"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho SHOULD_NOT_RUN\n")
	h := &Handler{}

	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated start = %d, want 401", rec.Code)
	}
	if st := readUpdateState(); st.Running {
		t.Fatal("an unauthenticated request started an update")
	}
	if log := readUpdateLog(); strings.Contains(log, "SHOULD_NOT_RUN") {
		t.Fatal("the stub script ran despite the 401")
	}
}

func TestUpdateStartRefusesWhileRunning(t *testing.T) {
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho SHOULD_NOT_RUN\n")
	h := &Handler{}

	// A run started moments ago by another tab/proxy: the lock must hold.
	if err := writeUpdateState(updateState{Running: true, StartedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	rec := httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("start while running = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if log := readUpdateLog(); strings.Contains(log, "SHOULD_NOT_RUN") {
		t.Fatal("the stub script ran despite the running lock")
	}
}

// A run killed mid-flight must not lock the operator out of ever updating again.
func TestUpdateStartIgnoresStaleRunningLock(t *testing.T) {
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho STUB_RAN\nexit 0\n")
	h := &Handler{}

	stale := time.Now().UTC().Add(-updateStaleAfter - time.Minute).Format(time.RFC3339)
	if err := writeUpdateState(updateState{Running: true, StartedAt: stale}); err != nil {
		t.Fatalf("seed stale state: %v", err)
	}

	rec := httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start with stale lock = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	st := waitForUpdateSettled(t, h)
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want 0", st.ExitCode)
	}
}

// A binary shipped without scripts/ must say so instead of half-starting.
func TestUpdateStartReportsMissingScript(t *testing.T) {
	chdirToTempUpdateTree(t, "") // no script written
	h := &Handler{}

	rec := httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("start without script = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if st := readUpdateState(); st.Running {
		t.Fatal("state left running after a missing-script refusal")
	}
}

// The happy path: the script runs detached, its output lands in the log file,
// and its exit status is recorded on disk — the only witness that outlives the
// restart the real script performs.
func TestUpdateStartRunsScriptAndRecordsExitCode(t *testing.T) {
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho STUB_LINE_ONE\necho STUB_LINE_TWO\nexit 0\n")
	h := &Handler{}

	rec := httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	st := waitForUpdateSettled(t, h)
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want 0", st.ExitCode)
	}
	logText := readUpdateLog()
	for _, want := range []string{"STUB_LINE_ONE", "STUB_LINE_TWO"} {
		if !strings.Contains(logText, want) {
			t.Errorf("log missing %q; got:\n%s", want, logText)
		}
	}

	// And the status endpoint must now report that same recorded result.
	rec = httptest.NewRecorder()
	h.apiGetUpdateStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/api/update/status", nil))
	var body struct {
		Running  bool   `json:"running"`
		ExitCode *int   `json:"exitCode"`
		Log      string `json:"log"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body.Running {
		t.Error("running = true after settle")
	}
	if body.ExitCode == nil || *body.ExitCode != 0 {
		t.Errorf("status exitCode = %v, want 0", body.ExitCode)
	}
	if !strings.Contains(body.Log, "STUB_LINE_ONE") {
		t.Errorf("status log missing script output; got:\n%s", body.Log)
	}
}

// A failing script must surface its non-zero exit code, which is what the UI
// turns into "update failed, proxy rolled back".
func TestUpdateStartRecordsFailureExitCode(t *testing.T) {
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho STUB_FAILED\nexit 7\n")
	h := &Handler{}

	rec := httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d, want 202", rec.Code)
	}

	st := waitForUpdateSettled(t, h)
	if st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("exitCode = %v, want 7", st.ExitCode)
	}
	if logText := readUpdateLog(); !strings.Contains(logText, "STUB_FAILED") {
		t.Errorf("log missing script output; got:\n%s", logText)
	}
}

// The 401 test proves auth runs first; it does NOT prove the routes are wired
// into the dispatch switch (auth rejects before the switch is ever reached). This
// walks the whole dispatcher with a valid token, which is the only thing that
// catches a route registered under the wrong path or method.
func TestUpdateRoutesReachHandlerThroughDispatch(t *testing.T) {
	initConfigForTests(t)
	if err := config.SetPassword("correct-horse-battery"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho STUB_VIA_DISPATCH\nexit 0\n")
	h := &Handler{}
	token := issueAdminTestToken(t)

	// GET /update/status must reach the handler, not fall through to a 404.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/update/status", nil)
	req.Header.Set("X-Admin-Token", token)
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /update/status via dispatch = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// POST /update/start must reach the handler and actually spawn.
	req = httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil)
	req.Header.Set("X-Admin-Token", token)
	rec = httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /update/start via dispatch = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	st := waitForUpdateSettled(t, h)
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want 0", st.ExitCode)
	}
	if logText := readUpdateLog(); !strings.Contains(logText, "STUB_VIA_DISPATCH") {
		t.Errorf("log missing script output; got:\n%s", logText)
	}
}

// The status route is read-only and must not be reachable as a state change.
func TestUpdateStartRejectsGET(t *testing.T) {
	initConfigForTests(t)
	if err := config.SetPassword("correct-horse-battery"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho SHOULD_NOT_RUN\n")
	h := &Handler{}
	token := issueAdminTestToken(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/update/start", nil)
	req.Header.Set("X-Admin-Token", token)
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code == http.StatusAccepted {
		t.Fatal("GET /update/start started an update; the route must be POST-only")
	}
	if log := readUpdateLog(); strings.Contains(log, "SHOULD_NOT_RUN") {
		t.Fatal("the stub script ran on a GET")
	}
}

// The spawned script must be told which version is ACTUALLY running, not left
// to read the git-tracked version.json. In repo layout that file describes the
// checkout: a release commit bumps it to the new tag while the service still
// runs the previous binary, so a script trusting the file decides "already up
// to date", exits 0, and the dashboard reports success without swapping
// anything — the operator sees "update complete" and stays on the old version.
func TestUpdateStartPassesRunningVersionToScript(t *testing.T) {
	// The stub echoes the variable it was handed, so the assertion is about what
	// the child process actually received, not about how we built the env slice.
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho \"SEEN=${OMNIPROXY_INSTALLED_VERSION:-<unset>}\"\n")
	initConfigForTests(t)

	prev := config.Version
	config.Version = "1.2.3"
	defer func() { config.Version = prev }()

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if st := waitForUpdateSettled(t, h); st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want 0", st.ExitCode)
	}
	if logText := readUpdateLog(); !strings.Contains(logText, "SEEN=1.2.3") {
		t.Errorf("script did not receive the running version; log:\n%s", logText)
	}

	// And the detail the whole fix turns on: the repo layout is set too, so the
	// script resolves binary/web beside the clone rather than in ~/.omniproxy-user.
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho \"LAYOUT=${OMNIPROXY_LAYOUT:-<unset>} PORT=${OMNIPROXY_PORT:-<unset>}\"\n")
	rec = httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second start = %d, want 202", rec.Code)
	}
	if st := waitForUpdateSettled(t, h); st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("second exitCode = %v, want 0", st.ExitCode)
	}
	logText := readUpdateLog()
	if !strings.Contains(logText, "LAYOUT=repo") {
		t.Errorf("repo layout not passed to the script; log:\n%s", logText)
	}
	// A prefix install can listen anywhere; a script left to its 8080 default
	// health-checks a port nothing serves and rolls a good swap back.
	if want := "PORT=" + strconv.Itoa(config.GetPort()); !strings.Contains(logText, want) {
		t.Errorf("listen port not passed to the script, want %q; log:\n%s", want, logText)
	}
}

// The service manager's PATH is not the developer's shell PATH. The macOS
// launchd job runs with /opt/homebrew/bin:/usr/local/bin:/usr/bin:/usr/sbin:/sbin
// — no /bin, which is where macOS keeps bash, date and launchctl. Resolving
// "bash" through that PATH made every one-click update answer 500
// (`exec: "bash": executable file not found in $PATH`) and swap nothing, while
// the stubbed tests above stayed green because `go test` inherits a normal PATH.
// update.sh also shells out to date and launchctl, so the child needs a usable
// PATH, not just a resolvable shell.
func TestUpdateStartWorksWithServiceManagerPath(t *testing.T) {
	// Stub runs `date`, which lives in /bin on macOS: proves the child PATH was
	// repaired, not merely that bash was found.
	chdirToTempUpdateTree(t, "#!/usr/bin/env bash\necho STUB_RAN_WITH_RESTRICTED_PATH\ndate -u +STUB_DATE_%Y\n")
	initConfigForTests(t)
	t.Setenv("PATH", "/usr/bin:/usr/sbin:/sbin") // the launchd PATH shape, minus /bin

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.apiStartUpdate(rec, httptest.NewRequest(http.MethodPost, "/admin/api/update/start", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start with a /bin-less PATH = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	st := waitForUpdateSettled(t, h)
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want 0; log:\n%s", st.ExitCode, readUpdateLog())
	}

	logText := readUpdateLog()
	if !strings.Contains(logText, "STUB_RAN_WITH_RESTRICTED_PATH") {
		t.Errorf("stub script did not run; log:\n%s", logText)
	}
	if !strings.Contains(logText, "STUB_DATE_") {
		t.Errorf("child PATH cannot reach date(1); log:\n%s", logText)
	}
	if strings.Contains(logText, "command not found") {
		t.Errorf("child PATH is missing a tool the updater needs; log:\n%s", logText)
	}
}

// A runaway script must not make the admin response unbounded.
func TestUpdateStatusTrimsOversizedLog(t *testing.T) {
	chdirToTempUpdateTree(t, "")
	if err := os.MkdirAll("data", 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	// One marker at the very start (must be trimmed away) and one at the end.
	var sb strings.Builder
	sb.WriteString("OLDEST_MARKER\n")
	for sb.Len() < updateLogMaxBytes*2 {
		sb.WriteString("filler line to pad the log past the cap\n")
	}
	sb.WriteString("NEWEST_MARKER\n")
	if err := os.WriteFile(updateLogFile, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write big log: %v", err)
	}

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.apiGetUpdateStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/api/update/status", nil))

	var body struct {
		Log string `json:"log"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Log) > updateLogMaxBytes+1024 {
		t.Errorf("log length = %d, want it capped near %d", len(body.Log), updateLogMaxBytes)
	}
	if !strings.Contains(body.Log, "NEWEST_MARKER") {
		t.Error("the newest output was dropped; trimming must keep the tail")
	}
	if strings.Contains(body.Log, "OLDEST_MARKER") {
		t.Error("the oldest output survived; trimming must drop the head")
	}
}

// TestDetectUpdateLayout locks the layout the updater installs into to the
// tree the running binary actually lives in. A fixed guess wrote the new
// binary and web/ one level above a prefix install, and the restarted service
// kept serving the untouched old binary.
func TestDetectUpdateLayout(t *testing.T) {
	t.Run("prefix install", func(t *testing.T) {
		home := t.TempDir()
		exeDir := filepath.Join(home, "bin")
		for _, d := range []string{exeDir, filepath.Join(exeDir, "web")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		// A prefix install claims its version in HOME, one level above bin/.
		if err := os.WriteFile(filepath.Join(home, "version.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		layout, got := detectUpdateLayout("/unrelated/cwd", exeDir)
		if layout != "prefix" || got != home {
			t.Fatalf("layout, home = %q, %q; want prefix, %q", layout, got, home)
		}
	})
	t.Run("repo install", func(t *testing.T) {
		repo := t.TempDir()
		// A git clone keeps version.json beside the binary in the repo root.
		if err := os.WriteFile(filepath.Join(repo, "version.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		layout, got := detectUpdateLayout(repo, repo)
		if layout != "repo" || got != repo {
			t.Fatalf("layout, home = %q, %q; want repo, %q", layout, got, repo)
		}
	})
	t.Run("fresh prefix install without version.json", func(t *testing.T) {
		home := t.TempDir()
		exeDir := filepath.Join(home, "bin")
		if err := os.MkdirAll(filepath.Join(exeDir, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		layout, got := detectUpdateLayout("/unrelated/cwd", exeDir)
		if layout != "prefix" || got != home {
			t.Fatalf("layout, home = %q, %q; want prefix, %q", layout, got, home)
		}
	})
}
