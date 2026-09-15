//go:build !unix

package daemon

import (
	"errors"
	"os/exec"
)

var errUnsupported = errors.New("daemon mode requires unix")

func detach(*exec.Cmd) error        { return errUnsupported }
func Start(...string) (bool, error) { return false, errUnsupported }
func Stop() (bool, error)           { return false, errUnsupported }
func PidFile() string               { return ".agent/agent-mcp.pid" }
func LogFile() string               { return ".agent/agent-mcp.log" }
