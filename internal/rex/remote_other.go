//go:build !windows

package rex

import "os/exec"

func hideWindow(*exec.Cmd) {}
