package platform

import (
	"fmt"
	"os"
	"strconv"
)

// ServeArgs are passed from the launcher to the elevated server process.
type ServeArgs struct {
	Port      int
	TokenFile string
	DataDir   string
	Home      string
	UID, GID  int
}

// Flags renders the arguments for the elevated child.
func (a ServeArgs) Flags() []string {
	return []string{"--serve", "--port", strconv.Itoa(a.Port), "--token-file", a.TokenFile,
		"--data-dir", a.DataDir, "--home", a.Home, "--uid", strconv.Itoa(a.UID), "--gid", strconv.Itoa(a.GID)}
}

// Exe returns the absolute path of the running program.
func Exe() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return p
}

// ChownToUser hands files created by the elevated process back to the user.
func ChownToUser(path string) {
	uid, _ := strconv.Atoi(os.Getenv("DRL_UID"))
	gid, _ := strconv.Atoi(os.Getenv("DRL_GID"))
	if uid > 0 {
		chownTree(path, uid, gid)
	}
}

func quoteSh(s string) string {
	out := "'"
	for _, c := range s {
		if c == '\'' {
			out += `'\''`
		} else {
			out += string(c)
		}
	}
	return out + "'"
}

var _ = fmt.Sprintf
