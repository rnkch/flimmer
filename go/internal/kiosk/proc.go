package kiosk

import (
	"os/exec"
	"syscall"
	"time"
)

// killGroupOnCancel runs cmd in its own process group and, when its context
// ends (cancel or shutdown), kills the whole group, not just the first
// process: children such as the second yt-dlp process or ffmpeg would
// otherwise keep running.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
}
