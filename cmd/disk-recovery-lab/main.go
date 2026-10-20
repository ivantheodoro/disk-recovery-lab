// Disk Recovery Lab: scan drives, browse what can be recovered, and copy
// the files you choose to another drive. Nothing is ever written to the
// drive being scanned.
//
// Launching the program normally asks for administrator rights (needed to
// read drives directly), starts a local server with those rights and opens
// the interface as the current user. Use --no-elevate to run without them
// (disk images only).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ivantheodoro/disk-recovery-lab/internal/platform"
	"github.com/ivantheodoro/disk-recovery-lab/internal/scan"
	"github.com/ivantheodoro/disk-recovery-lab/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		serve     = flag.Bool("serve", false, "run the server only (used by the launcher after elevation)")
		noElevate = flag.Bool("no-elevate", false, "do not ask for administrator rights (disk images only)")
		noUI      = flag.Bool("no-ui", false, "do not open a window; print the URL instead")
		port      = flag.Int("port", 0, "port to listen on (default: a free port)")
		tokenFile = flag.String("token-file", "", "file holding the session token (internal)")
		dataDir   = flag.String("data-dir", "", "where scans and images are stored")
		home      = flag.String("home", "", "home folder of the user (internal)")
		uid       = flag.Int("uid", -1, "user id to hand created files to (internal)")
		gid       = flag.Int("gid", -1, "group id (internal)")
		showVer   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("Disk Recovery Lab", version)
		return
	}
	scan.FreeSpace = platform.FreeSpace
	if *serve {
		os.Exit(runServer(*port, *tokenFile, *dataDir, *home, *uid, *gid, false))
	}
	launch(*port, *dataDir, *noElevate, *noUI)
}

func defaultDataDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Disk Recovery Lab")
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Disk Recovery Lab")
		}
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && os.Geteuid() != 0 {
		return filepath.Join(d, "disk-recovery-lab")
	}
	return filepath.Join(home, ".local", "share", "disk-recovery-lab")
}

func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 8765
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func waitUp(port int) bool {
	for i := 0; i < 120; i++ {
		if r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port)); err == nil {
			r.Body.Close()
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func launch(port int, dataDir string, noElevate, noUI bool) {
	uid, gid, home := platform.CurrentUser()
	if dataDir == "" {
		dataDir = defaultDataDir(home)
	}
	if port == 0 {
		port = freePort()
	}
	token := newToken()
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, token)
	open := func() {
		if noUI {
			fmt.Println("Disk Recovery Lab is running. Open:", url)
			return
		}
		if err := platform.OpenUI(url); err != nil {
			fmt.Println("Open this address in your browser:", url)
		}
	}
	if !platform.IsAdmin() && !noElevate {
		tf, err := server.WriteTokenFile(token)
		if err == nil {
			err = platform.Elevate(platform.ServeArgs{Port: port, TokenFile: tf, DataDir: dataDir, Home: home, UID: uid, GID: gid})
			if err == nil && waitUp(port) {
				open()
				return // the elevated server keeps running on its own
			}
			os.Remove(tf)
		}
		fmt.Fprintln(os.Stderr, "Running without administrator rights (disk images only):", err)
	}
	// Serve in this process: already elevated, or the user declined.
	tf, _ := server.WriteTokenFile(token)
	go func() {
		if waitUp(port) {
			open()
		}
	}()
	os.Exit(runServer(port, tf, dataDir, home, uid, gid, true))
}

func runServer(port int, tokenFile, dataDir, home string, uid, gid int, inLauncher bool) int {
	tok, err := os.ReadFile(tokenFile)
	os.Remove(tokenFile)
	if err != nil || len(tok) < 16 {
		fmt.Fprintln(os.Stderr, "missing session token")
		return 2
	}
	token := strings.TrimSpace(string(tok))
	if home != "" {
		os.Setenv("DRL_USER_HOME", home)
	}
	if uid >= 0 {
		os.Setenv("DRL_UID", strconv.Itoa(uid))
		os.Setenv("DRL_GID", strconv.Itoa(gid))
	}
	if dataDir == "" {
		dataDir = defaultDataDir(platform.HomeDir())
	}
	srv := server.New(server.Options{Port: port, Token: token, DataDir: dataDir, Version: version})
	ln, err := srv.Listen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot listen:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := srv.Serve(ctx, ln); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if srv.Restart { // same rights, same port and token: the open window just reloads
		tf, err := server.WriteTokenFile(token)
		if err != nil {
			return 1
		}
		args := []string{"--serve", "--port", strconv.Itoa(port), "--token-file", tf, "--data-dir", dataDir,
			"--home", home, "--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid)}
		restart(args)
	}
	return 0
}

func restart(args []string) {
	cmd := exec.Command(platform.Exe(), args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "restart failed:", err)
	}
}
