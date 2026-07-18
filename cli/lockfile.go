package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func PIDPath(configPath string) string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "data", "omniproxy.pid")
	}
	return filepath.Join(filepath.Dir(configPath), "omniproxy.pid")
}

func CheckAndKillExisting(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		os.Remove(pidPath)
		return false
	}

	if !platformProcessAlive(pid) {
		os.Remove(pidPath)
		return false
	}

	fmt.Printf("  Killing existing OmniProxy (PID: %d)...\n", pid)

	platformSendTermination(pid)
	time.Sleep(500 * time.Millisecond)

	if platformProcessAlive(pid) {
		platformForceKill(pid)
		time.Sleep(200 * time.Millisecond)
	}

	os.Remove(pidPath)
	fmt.Println("  OmniProxy is killed.")
	return true
}

func WritePID(pidPath string) error {
	if err := os.MkdirAll(filepath.Dir(pidPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
}

func RemovePID(pidPath string) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid != os.Getpid() {
		return
	}
	os.Remove(pidPath)
}
