package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"omniproxy/config"
)

// The update flow spans a proxy restart: update.sh swaps the binary in and then
// restarts the service, killing the very process that spawned it. So every piece
// of state lives on DISK — the log and the state file — never in memory, because
// whatever is in memory dies at exactly the moment the client most needs to know
// what happened. A freshly restarted proxy reads the same two files and reports
// the update honestly.
const (
	updateStateFile = "data/update.state"
	updateLogFile   = "data/update.log"
	// An update cannot legitimately outlive this: the script caps curl at 300s,
	// gives the health check 30s and its rollback health check 20s. Past this
	// bound a "running" flag can only be a run that was killed mid-flight, so
	// the lock is treated as stale rather than blocking updates forever.
	updateStaleAfter = 15 * time.Minute
	// The log is small (a few dozen lines), but never trust that: cap what is
	// read and sent so a runaway script cannot balloon an admin response.
	updateLogMaxBytes = 256 * 1024
)

type updateState struct {
	Running   bool   `json:"running"`
	StartedAt string `json:"startedAt,omitempty"`
	ExitCode  *int   `json:"exitCode"`
}

// updateStartMu serialises the check-then-spawn in this process, which is the
// only concurrency that actually occurs (two admin tabs, a double click). A
// second proxy process cannot race it: while a run is live the state file says
// so, and that file survives the restart.
var updateStartMu sync.Mutex

func updateAbsPath(rel string) string {
	abs, err := filepath.Abs(rel)
	if err != nil {
		return rel
	}
	return abs
}

func readUpdateState() updateState {
	var st updateState
	data, err := os.ReadFile(updateStateFile)
	if err != nil {
		return st
	}
	if json.Unmarshal(data, &st) != nil {
		return updateState{}
	}
	if st.Running {
		started, err := time.Parse(time.RFC3339, st.StartedAt)
		if err != nil || time.Since(started) > updateStaleAfter {
			// A run that never finished: report it as finished-unknown so the
			// operator is not locked out of retrying.
			st.Running = false
		}
	}
	return st
}

func writeUpdateState(st updateState) error {
	if err := os.MkdirAll(filepath.Dir(updateStateFile), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := updateStateFile + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, updateStateFile)
}

func readUpdateLog() string {
	f, err := os.Open(updateLogFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > updateLogMaxBytes {
		if _, err := f.Seek(info.Size()-updateLogMaxBytes, io.SeekStart); err == nil {
			// Drop the partial first line so the client never renders half a line.
			buf := make([]byte, updateLogMaxBytes)
			n, _ := io.ReadFull(f, buf)
			text := string(buf[:n])
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = "[update] …earlier output trimmed…\n" + text[i+1:]
			}
			return text
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// resolveUpdateScript locates scripts/update.sh without ever taking a path from
// the request. Candidates in order: an explicit operator override, beside the
// working directory (a git clone, where the service starts in the repo root),
// and beside the executable (a tarball install, where the service manager does
// not guarantee the cwd is the install root).
func resolveUpdateScript() (string, bool) {
	var candidates []string
	if override := os.Getenv("OMNIPROXY_UPDATE_SCRIPT"); override != "" {
		candidates = append(candidates, override)
	}
	candidates = append(candidates, filepath.Join("scripts", "update.sh"))
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "scripts", "update.sh"))
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			return abs, true
		}
	}
	return "", false
}

// detectUpdateLayout reports which scripts/update.sh layout matches where this
// binary actually lives, so the updater installs into the tree the service
// executes. A prefix install (systemd, ~/.omniproxy-user) keeps the binary in
// HOME/bin beside the installed web/ or scripts/, with version.json one level
// up in HOME; a repo install (launchd, git clone) keeps version.json beside the
// binary itself. Getting this wrong is silent and costly: the script writes the
// new binary where nothing executes it and the restarted service keeps serving
// the old one.
func detectUpdateLayout(wd, exeDir string) (layout, home string) {
	_, webHere := os.Stat(filepath.Join(exeDir, "web"))
	_, scriptsHere := os.Stat(filepath.Join(exeDir, "scripts"))
	_, versionHere := os.Stat(filepath.Join(exeDir, "version.json"))
	if (webHere == nil || scriptsHere == nil) && versionHere != nil {
		return "prefix", filepath.Clean(filepath.Join(exeDir, ".."))
	}
	return "repo", wd
}

// apiGetUpdateStatus GET /admin/api/update/status
// Reports the persisted state of the last/current update. Reads only from disk,
// so it answers correctly from the proxy process that came up mid-update.
func (h *Handler) apiGetUpdateStatus(w http.ResponseWriter, r *http.Request) {
	st := readUpdateState()
	resp := map[string]interface{}{
		"running":   st.Running,
		"startedAt": st.StartedAt,
		"exitCode":  st.ExitCode,
		"log":       readUpdateLog(),
		"current":   config.Version,
	}
	json.NewEncoder(w).Encode(resp)
}

// systemDirs are the directories a POSIX system keeps bash, date and launchctl
// in. A service manager is free to hand the job a PATH without them: the macOS
// launchd job for OmniProxy runs with
// /opt/homebrew/bin:/usr/local/bin:/usr/bin:/usr/sbin:/sbin — no /bin, which is
// where macOS keeps all three. exec.Command("bash") resolves through the PARENT
// PATH, so every one-click update answered 500 with `exec: "bash": executable
// file not found in $PATH` and swapped nothing. The stubbed tests stayed green
// because `go test` inherits an ordinary shell PATH, which is exactly the blind
// spot this list closes. update.sh also shells out to date and launchctl, so the
// child needs a PATH that reaches them, not merely a resolvable shell.
var systemDirs = []string{"/bin", "/usr/bin", "/usr/sbin", "/sbin"}

// withSystemPath returns env with every systemDirs entry that PATH is missing
// appended to it. Existing entries keep their order, so an operator's PATH still
// wins; the suffix only fills in what a trimmed service-manager PATH dropped.
func withSystemPath(env []string) []string {
	path, found := "", false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path, found = strings.TrimPrefix(kv, "PATH="), true
			break
		}
	}
	have := make(map[string]bool)
	for _, d := range filepath.SplitList(path) {
		have[d] = true
	}
	missing := make([]string, 0, len(systemDirs))
	for _, d := range systemDirs {
		if !have[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return env
	}
	joined := strings.Join(missing, string(os.PathListSeparator))
	if found {
		path += string(os.PathListSeparator) + joined
	} else {
		path = joined
	}

	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if !replaced && strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+path)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+path)
	}
	return out
}

// resolveBash returns an absolute path to bash, or "" if none is found.
//
// This cannot be left to exec.Command("bash", ...): Go resolves that name through
// the PARENT process's PATH at Start time, and cmd.Env does not influence it. The
// launchd job's PATH has no /bin (where macOS keeps bash), so the spawn failed
// with `exec: "bash": executable file not found in $PATH` no matter what the
// child environment said. Searching explicitly — the operator's PATH first, then
// the POSIX system directories — makes the lookup independent of whatever PATH
// the service manager happened to pass.
func resolveBash() string {
	dirs := filepath.SplitList(os.Getenv("PATH"))
	dirs = append(dirs, systemDirs...)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, "bash")
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// apiStartUpdate POST /admin/api/update/start
// Spawns scripts/update.sh detached from this process's session, with all state
// on disk. Takes no input from the request: the script path is derived from the
// working directory, so there is no value a client could inject.
func (h *Handler) apiStartUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	updateStartMu.Lock()
	defer updateStartMu.Unlock()

	if st := readUpdateState(); st.Running {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "An update is already running"})
		return
	}

	script, found := resolveUpdateScript()
	if !found {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "scripts/update.sh not found beside the working directory or the executable; this build cannot self-update",
		})
		return
	}

	if err := os.MkdirAll(filepath.Dir(updateLogFile), 0o755); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "cannot create data directory: " + err.Error()})
		return
	}
	// Truncate: this run's output is this run's story.
	logFile, err := os.OpenFile(updateLogFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "cannot open update log: " + err.Error()})
		return
	}
	defer logFile.Close()

	started := time.Now().UTC()
	if err := writeUpdateState(updateState{Running: true, StartedAt: started.Format(time.RFC3339)}); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "cannot record update state: " + err.Error()})
		return
	}

	// Resolved before the state is committed as running: if there is no shell to
	// spawn, this must fail as a plain refusal rather than leave a "running" flag
	// behind for the next status poll to misread.
	bashPath := resolveBash()
	if bashPath == "" {
		_ = writeUpdateState(updateState{Running: false})
		fmt.Fprintf(logFile, "[update] ERROR: no bash found in PATH or %s\n", strings.Join(systemDirs, ", "))
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "no bash found; cannot run the update script"})
		return
	}

	statePath := updateAbsPath(updateStateFile)
	wd, _ := os.Getwd()

	// Fixed wrapper — paths are passed as arguments ($1/$2/$3), never interpolated
	// into the program text, so none of them can break out into the shell. It
	// records the exit status itself because this proxy process will be killed by
	// the restart before it could: the wrapper is the only witness that outlives
	// the swap. It invokes the shell by absolute path ($3) rather than the name
	// "bash", which would be looked up in a PATH the service manager may have
	// trimmed of /bin.
	const wrapper = `set +e
"$3" "$1"
code=$?
printf '{"running":false,"exitCode":%d,"finishedAt":"%s"}\n' "$code" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$2"
printf '\n[update] script exited with code %d\n' "$code"
`
	cmd := exec.Command(bashPath, "-c", wrapper, "omniproxy-update", script, statePath, bashPath)
	cmd.Dir = wd
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Repair the PATH first: a service manager can hand the job a PATH without
	// /bin, and bash itself lives there on macOS (see systemDirs).
	exe, _ := os.Executable()
	layout, home := detectUpdateLayout(wd, filepath.Dir(exe))
	cmd.Env = withSystemPath(append(os.Environ(),
		// The layout follows where this binary actually lives, never a fixed
		// guess: hardcoding one made the installer write the new binary and
		// web/ one level above a prefix-layout install (~/.omniproxy-user/
		// omniproxy instead of .../bin/omniproxy), and the restarted service
		// kept serving the untouched old binary.
		"OMNIPROXY_LAYOUT="+layout,
		fmt.Sprintf("OMNIPROXY_HOME=%s", home),
	))
	// The running binary's own version, so update.sh compares against what is
	// actually executing rather than the git-tracked version.json. In repo layout
	// that file describes the CHECKOUT, not the process: after a release bump
	// commits 0.6.1 the file already says 0.6.1 while this process still runs the
	// 0.6.0 it started as, so the script would decide "already up to date", exit 0,
	// and the dashboard would report a successful update that swapped nothing —
	// leaving the operator in a "new version available / update complete" loop.
	// config.Version is read once at startup, which is exactly the fact needed.
	// Omitted when empty so the script's own version.json fallback still applies.
	if config.Version != "" {
		cmd.Env = append(cmd.Env, "OMNIPROXY_INSTALLED_VERSION="+config.Version)
	}
	// Detach into its own session and process group. Without this the restart
	// (launchctl kickstart -k) takes the script down with the job's process
	// group — killing the updater halfway through swapping binaries.
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		_ = writeUpdateState(updateState{Running: false})
		fmt.Fprintf(logFile, "[update] ERROR: failed to start update script: %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to start update: " + err.Error()})
		return
	}
	// Deliberately not cmd.Wait(): this process is about to be replaced.
	// Wait is called in a goroutine purely so the child is reaped if the
	// restart happens to leave this process alive (e.g. a failed swap).
	go func() { _ = cmd.Wait() }()

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "started",
		"startedAt": started.Format(time.RFC3339),
	})
}
