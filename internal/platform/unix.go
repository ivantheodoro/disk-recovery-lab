//go:build darwin || linux

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// IsAdmin reports whether raw drives can be opened.
func IsAdmin() bool { return os.Geteuid() == 0 }

// asUser runs a command as the person who launched the app, so a browser or
// Finder window never runs with administrator rights.
func asUser(name string, args ...string) *exec.Cmd {
	uid := os.Getenv("DRL_UID")
	if uid == "" {
		uid = os.Getenv("SUDO_UID")
	}
	if os.Geteuid() != 0 || uid == "" || uid == "0" {
		return exec.Command(name, args...)
	}
	if isDarwin {
		return exec.Command("launchctl", append([]string{"asuser", uid, "sudo", "-u", "#" + uid, "--", name}, args...)...)
	}
	return exec.Command("sudo", append([]string{"-u", "#" + uid, "--preserve-env=DISPLAY,WAYLAND_DISPLAY,XDG_RUNTIME_DIR,DBUS_SESSION_BUS_ADDRESS", "--", name}, args...)...)
}

func chownTree(path string, uid, gid int) {
	filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			os.Lchown(p, uid, gid)
		}
		return nil
	})
	for d := filepath.Dir(path); strings.HasPrefix(d, HomeDir()) && d != HomeDir(); d = filepath.Dir(d) {
		os.Lchown(d, uid, gid)
	}
}

// CurrentUser returns uid, gid and home of the person launching the app.
func CurrentUser() (int, int, string) {
	if u := os.Getenv("SUDO_UID"); u != "" && os.Geteuid() == 0 {
		uid, _ := strconv.Atoi(u)
		gid, _ := strconv.Atoi(os.Getenv("SUDO_GID"))
		home := "/home/" + os.Getenv("SUDO_USER")
		if isDarwin {
			home = "/Users/" + os.Getenv("SUDO_USER")
		}
		return uid, gid, home
	}
	h, _ := os.UserHomeDir()
	return os.Getuid(), os.Getgid(), h
}
