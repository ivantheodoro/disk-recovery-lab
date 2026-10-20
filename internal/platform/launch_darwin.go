//go:build darwin

package platform

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

const isDarwin = true

// Elevate starts the server with administrator rights using the standard
// macOS password prompt. It returns once the server has been started.
func Elevate(a ServeArgs) error {
	cmd := quoteSh(Exe())
	for _, f := range a.Flags() {
		cmd += " " + quoteSh(f)
	}
	cmd += " >/dev/null 2>&1 &"
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(cmd)
	script := `do shell script "` + esc + `" with prompt "Disk Recovery Lab needs administrator rights to read drives directly. It never writes to the drive you scan." with administrator privileges`
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-128") {
			return errors.New("cancelled")
		}
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

var appBrowsers = []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium", "Arc"}

// OpenUI opens the interface in its own app window when a Chromium-based
// browser is installed, otherwise in the default browser.
func OpenUI(url string) error {
	for _, b := range appBrowsers {
		if _, err := os.Stat("/Applications/" + b + ".app"); err == nil {
			return asUser("open", "-na", b, "--args", "--app="+url).Run()
		}
	}
	return asUser("open", url).Run()
}
