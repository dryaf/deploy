package workerctl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type Prompter interface {
	Ask(context.Context, string, bool) (string, error)
}

type TerminalPrompt struct {
	Input  *os.File
	Output io.Writer
}

func (p TerminalPrompt) Ask(ctx context.Context, label string, secret bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.Input == nil || p.Output == nil {
		return "", fmt.Errorf("interactive input unavailable")
	}
	fd := int(p.Input.Fd())
	before, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", fmt.Errorf("terminal required; supply configuration inputs for unattended use")
	}
	if secret {
		after := *before
		after.Lflag &^= unix.ECHO
		if err = unix.IoctlSetTermios(fd, unix.TCSETS, &after); err != nil {
			return "", fmt.Errorf("cannot disable terminal echo")
		}
		defer unix.IoctlSetTermios(fd, unix.TCSETS, before)
	}
	fmt.Fprint(p.Output, label+": ")
	// Read one byte at a time: no buffered reader may consume the following answer
	// or interfere with an inherited helper protocol. Terminal input is bounded.
	var body []byte
	one := make([]byte, 1)
	for len(body) <= 4096 {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		ready, pollErr := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 100)
		if pollErr == unix.EINTR {
			continue
		}
		if pollErr != nil {
			return "", fmt.Errorf("terminal input unavailable")
		}
		if ready == 0 {
			continue
		}
		n, readErr := p.Input.Read(one)
		if readErr != nil || n == 0 {
			return "", fmt.Errorf("input cancelled or closed")
		}
		if one[0] == '\n' {
			if secret {
				fmt.Fprintln(p.Output)
			}
			return strings.TrimSpace(string(body)), nil
		}
		if one[0] == '\x03' || one[0] == '\x04' {
			return "", fmt.Errorf("input cancelled")
		}
		body = append(body, one[0])
	}
	return "", fmt.Errorf("input exceeds limit")
}

// StreamPrompt is injectable for scripted tests, never selected automatically for
// non-TTY stdin. Secrets are consumed but not echoed to Output.
type StreamPrompt struct {
	Input  *bufio.Reader
	Output io.Writer
}

func (p StreamPrompt) Ask(ctx context.Context, label string, secret bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.Input == nil {
		return "", io.EOF
	}
	if p.Output != nil {
		fmt.Fprintln(p.Output, label)
	}
	line, err := p.Input.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("input cancelled or closed")
	}
	if len(line) > 4096 {
		return "", fmt.Errorf("input exceeds limit")
	}
	return strings.TrimSpace(line), nil
}
