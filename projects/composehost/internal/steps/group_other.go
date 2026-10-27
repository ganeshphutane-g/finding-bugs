//go:build !unix

package steps

import "os/exec"

func inGroup(cmd *exec.Cmd) {}
