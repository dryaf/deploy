package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// CommandRequest travels over a private pipe, never through a shell. The caller
// supplies the already retained account lease and the opened, pinned executable
// as descriptors 3 and 4. Only a dedicated subprocess may run this supervisor:
// becoming a subreaper and collecting children must not affect other services.
type CommandRequest struct {
	Version int      `json:"version"`
	CLIHash string   `json:"cli_hash"`
	State   string   `json:"state"`
	Args    []string `json:"args"`
}

func validateCommandExecutable(f *os.File, hash string) error {
	if !safefs.DigestPattern.MatchString(hash) {
		return fmt.Errorf("Signal CLI digest required")
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) || info.Size() < 4 || info.Size() > 512<<20 {
		return fmt.Errorf("unsafe pinned Signal executable")
	}
	var magic [4]byte
	if _, err = f.ReadAt(magic[:], 0); err != nil || string(magic[:]) != "\x7fELF" {
		return fmt.Errorf("managed Signal requires a native ELF executable")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.NewSectionReader(f, 0, info.Size())); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return fmt.Errorf("pinned Signal executable changed")
	}
	return nil
}

// SuperviseCommand is the internal isolated child entry point. On every exit
// path it kills and reaps surviving descendants before releasing descriptor 3.
// Even a CLI which exits successfully while leaving a background child is a
// failed command. An uninterruptible child keeps the supervisor (and lease)
// alive, instead of falsely permitting a second account owner.
func SuperviseCommand(ctx context.Context, input io.Reader, lease, cli *os.File) error {
	raw, err := io.ReadAll(io.LimitReader(input, safefs.MaxRecordBytes+1))
	if err != nil {
		return err
	}
	var request CommandRequest
	if err = safefs.DecodeJSON(raw, &request, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if request.Version != 1 || !filepath.IsAbs(request.State) || filepath.Clean(request.State) != request.State || len(request.Args) < 3 || len(request.Args) > 64 || request.Args[0] != "--config" || request.Args[1] != request.State {
		return fmt.Errorf("invalid managed Signal command")
	}
	if lease == nil || cli == nil {
		return fmt.Errorf("Signal command descriptors required")
	}
	if _, err = lease.Stat(); err != nil {
		return err
	}
	if err = validateCommandExecutable(cli, request.CLIHash); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	path := "/proc/self/fd/4"
	p, err := os.StartProcess(path, append([]string{"signal-cli"}, request.Args...), &os.ProcAttr{
		Env:   []string{"PATH=/usr/bin:/bin", "HOME=" + request.State, "LANG=C.UTF-8"},
		Files: []*os.File{devnull, os.Stdout, os.Stderr, lease, cli},
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		return fmt.Errorf("cannot start pinned Signal CLI")
	}
	pid := p.Pid
	defer p.Release()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var result error
	parentDone, stopping, descendants := false, false, false
	for {
		for {
			var status unix.WaitStatus
			got, waitErr := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if waitErr == unix.EINTR {
				continue
			}
			if waitErr == unix.ECHILD {
				if !parentDone {
					return fmt.Errorf("Signal command lost its child")
				}
				if descendants && result == nil {
					result = fmt.Errorf("Signal CLI left surviving descendants")
				}
				return result
			}
			if waitErr != nil {
				result = fmt.Errorf("cannot join Signal CLI")
				stopping = true
				break
			}
			if got == 0 {
				break
			}
			if got == pid {
				parentDone, stopping = true, true
				if !status.Exited() || status.ExitStatus() != 0 {
					result = fmt.Errorf("Signal CLI failed")
				}
			} else {
				descendants = true
			}
		}
		if stopping {
			_ = unix.Kill(-pid, unix.SIGKILL)
			// Descendants which started another session are reparented here once
			// their parent exits. Discover them on every thread: Go may fork on
			// any OS thread. Killing then reaping repeats until ECHILD.
			if err := killSupervisorChildren(); err != nil && result == nil {
				result = err
			}
		}
		select {
		case <-ctx.Done():
			if result == nil {
				result = ctx.Err()
			}
			stopping = true
		case <-timer.C:
			if result == nil {
				result = context.DeadlineExceeded
			}
			stopping = true
		case <-ticker.C:
		}
		if stopping {
			// A cancelled context stays ready; keep cleanup polling bounded.
			<-ticker.C
		}
	}
}

func killSupervisorChildren() error {
	paths, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil {
		return err
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, field := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 {
				return fmt.Errorf("invalid Signal descendant identity")
			}
			// A direct unreaped child cannot have its PID reused here.
			if err = unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
				return err
			}
		}
	}
	return nil
}
