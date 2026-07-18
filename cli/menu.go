package cli

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"omniproxy/logger"
	"syscall"
)

func showMenu(addr string, pidPath string, shutdown func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		clearScreen()
		_, port, _ := net.SplitHostPort(addr)
		displayURL := fmt.Sprintf("http://localhost:%s", port)

		fmt.Println()
		fmt.Println(" ══════════════════════════════════════════")
		fmt.Printf("               OmniProxy                   \n")
		fmt.Printf("           %-28s \n", displayURL)
		fmt.Println(" ══════════════════════════════════════════")
		fmt.Println()
		fmt.Println("  Commands:")
		fmt.Println()
		fmt.Println("    [1] Open Web UI")
		fmt.Println("    [2] Run in Background")
		fmt.Println("    [3] Show Verbose Log")
		fmt.Println("    [4] Exit")
		fmt.Println()
		fmt.Print("  Enter command: ")

		input := readLine(sigCh)
		input = strings.TrimSpace(input)

		switch input {
		case "1":
			openBrowser(fmt.Sprintf("%s/admin", displayURL))
			pause(fmt.Sprintf("  Press Enter to go back to menu...Open browser manually: %s/admin", displayURL))

		case "2":
			// Release the port first so the daemon child can claim it.
			shutdown()
			spawnBackground(pidPath)
			return

		case "3":
			showLogViewer()

		case "4", "q", "":
			clearScreen()
			logger.Infof("Shutting down by user request...")
			shutdown()
			return

		default:
			pause("\n  Invalid command. Press Enter to try again...")
		}
	}
}

func readLine(sigCh chan os.Signal) string {
	done := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		done <- line
	}()
	select {
	case line := <-done:
		return line
	case <-sigCh:
		return "4"
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func spawnBackground(pidPath string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("\n  Cannot determine executable path: %v\n", err)
		pause("\n  Press Enter to return to menu...")
		return
	}

	os.Remove(pidPath)

	args := []string{"--daemon"}
	for _, a := range os.Args[1:] {
		if a != "--menu" {
			args = append(args, a)
		}
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	// Redirect to NUL on Windows, /dev/null on Unix so child doesn't
	// inherit parent's console handles. With DETACHED_PROCESS on Windows,
	// inherited console handles become stale when the parent exits.
	cmd.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	cmd.Stderr, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Printf("\n  Failed to start background process: %v\n", err)
		pause("\n  Press Enter to return to menu...")
		return
	}

	fmt.Printf("\n  OmniProxy running in background (PID: %d)\n", cmd.Process.Pid)
}

func pause(msg string) {
	fmt.Print(msg)
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func showLogViewer() {
	clearScreen()
	for {
		lines := logger.LogBuf.Lines()
		rows := 28
		offset := 0
		if len(lines) > rows {
			offset = len(lines) - rows
		}

		fmt.Println(" ── Verbose Log ────────────────────────────────")
		fmt.Println()

		if len(lines) == 0 {
			fmt.Println("  (no log entries yet)")
		} else {
			for i := offset; i < len(lines); i++ {
				fmt.Printf("  %s\n", strings.TrimRight(lines[i], "\n\r"))
			}
		}

		fmt.Println()
		fmt.Println(" ─────────────────────────────────────────────────")
		fmt.Print("  [r] refresh  [q] back: ")

		cmd, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		cmd = strings.TrimSpace(cmd)

		switch cmd {
		case "q", "":
			return
		case "r":
			clearScreen()
		}
	}
}

