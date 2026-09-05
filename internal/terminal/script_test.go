package terminal

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tmuxStub records every argv it is invoked with. Setting WMUX_TMUX_MISSING
// makes has-session fail, which is how a vanished remote session looks.
const tmuxStub = `#!/bin/sh
{
  for argument in "$@"; do
    printf '<%s>' "$argument"
  done
  printf '\n'
} >> "$WMUX_TMUX_LOG"
for argument in "$@"; do
  if [ "$argument" = has-session ] && [ -n "$WMUX_TMUX_MISSING" ]; then
    exit 1
  fi
done
exit 0
`

// TestRemoteScriptsRunUnderPOSIXAndFishLoginShells executes the exact command
// strings wmux hands to ssh.Session.Start. sshd runs them through the account's
// login shell, so they must survive a shell that is not POSIX-compatible.
func TestRemoteScriptsRunUnderPOSIXAndFishLoginShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the remote scripts are POSIX shell")
	}
	shells := map[string]string{}
	posix, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not installed")
	}
	shells["sh"] = posix
	if fish, err := exec.LookPath("fish"); err == nil {
		shells["fish"] = fish
	} else {
		t.Log("fish is not installed; only the POSIX login shell is exercised")
	}

	l := newLauncher(Config{MuxName: "wmux script test"})
	name := backendName("ses_script")
	spec := SessionSpec{
		ID:    "ses_script",
		Cwd:   "~/work/a'b",
		Shell: "/bin/sh",
		Args:  []string{"-lc", "make watch"},
		Env:   map[string]string{"WMUX_SESSION_ID": "ses_script"},
	}

	for shellName, shellPath := range shells {
		t.Run(shellName, func(t *testing.T) {
			run := func(t *testing.T, command string, missing bool) (string, int) {
				t.Helper()
				dir := t.TempDir()
				log := filepath.Join(dir, "tmux.log")
				stub := filepath.Join(dir, "tmux")
				if err := os.WriteFile(stub, []byte(tmuxStub), 0o700); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(shellPath, "-c", command)
				cmd.Env = []string{
					"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
					"HOME=" + dir,
					"WMUX_TMUX_LOG=" + log,
				}
				if missing {
					cmd.Env = append(cmd.Env, "WMUX_TMUX_MISSING=1")
				}
				output, err := cmd.CombinedOutput()
				status := 0
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					status = exitErr.ExitCode()
				} else if err != nil {
					t.Fatalf("run %s: %v (output %q)", shellName, err, output)
				}
				contents, readErr := os.ReadFile(log)
				if readErr != nil && !os.IsNotExist(readErr) {
					t.Fatal(readErr)
				}
				t.Logf("%s output=%q status=%d invocations=%q", shellName, output, status, contents)
				return string(contents) + "\n---output---\n" + string(output), status
			}

			t.Run("probe", func(t *testing.T) {
				_, status := run(t, posixScript("command -v tmux >/dev/null 2>&1"), false)
				if status != 0 {
					t.Fatalf("probe exit status = %d, want 0", status)
				}
			})

			t.Run("create and attach", func(t *testing.T) {
				recorded, status := run(t, posixScript(l.remoteAttachCommand(spec, PersistenceTmux, name, true)), true)
				if status != 0 {
					t.Fatalf("attach exit status = %d, want 0; recorded %q", status, recorded)
				}
				for _, wanted := range []string{
					"<has-session><-t><=" + name + ">",
					"<set-option><-g><update-environment><",
					"><;><new-session><-d><-s><" + name + ">",
					"XAUTHORITY COLORTERM WMUX_SESSION_ID>",
					"<'/bin/sh' '-lc' 'make watch'>",
					"<set-option><-g><mouse><on>",
					"<set-option><-as><terminal-overrides><,xterm*:Tc>",
					"<attach-session><-t><=" + name + ">",
				} {
					if !strings.Contains(recorded, wanted) {
						t.Fatalf("tmux invocations %q do not contain %q", recorded, wanted)
					}
				}
			})

			t.Run("attach only reports a missing session", func(t *testing.T) {
				recorded, status := run(t, posixScript(l.remoteAttachCommand(spec, PersistenceTmux, name, false)), true)
				if status != remoteMissingExitStatus {
					t.Fatalf("attach-only exit status = %d, want %d; recorded %q", status, remoteMissingExitStatus, recorded)
				}
				if !strings.Contains(recorded, "wmux: session "+name+" no longer exists on this host") {
					t.Fatalf("attach-only output %q does not explain the missing session", recorded)
				}
				if strings.Contains(recorded, "<new-session>") {
					t.Fatalf("attach-only created a session: %q", recorded)
				}
			})

			t.Run("attach only reuses an existing session", func(t *testing.T) {
				recorded, status := run(t, posixScript(l.remoteAttachCommand(spec, PersistenceTmux, name, false)), false)
				if status != 0 {
					t.Fatalf("attach-only exit status = %d, want 0; recorded %q", status, recorded)
				}
				if strings.Contains(recorded, "<new-session>") {
					t.Fatalf("attach-only created a session: %q", recorded)
				}
				if !strings.Contains(recorded, "<attach-session><-t><="+name+">") {
					t.Fatalf("attach-only did not attach: %q", recorded)
				}
			})

			t.Run("terminate", func(t *testing.T) {
				recorded, status := run(t, posixScript(l.remoteTerminateCommand(PersistenceTmux, name)), false)
				if status != 0 {
					t.Fatalf("terminate exit status = %d, want 0; recorded %q", status, recorded)
				}
				if !strings.Contains(recorded, "<kill-session><-t><="+name+">") {
					t.Fatalf("terminate invocations %q do not kill the exact session", recorded)
				}
			})
		})
	}
}
