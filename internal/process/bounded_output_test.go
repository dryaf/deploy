package process

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBoundedOutputCannotBypassWrite(t *testing.T) {
	// io.Copy may select ReaderFrom/WriterTo optimizations; neither may bypass
	// the byte limit. In particular, embedding bytes.Buffer would expose ReadFrom.
	var output Buffer
	output.Limit = 8
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() { defer w.Close(); _, _ = w.Write([]byte(strings.Repeat("x", 128))) }()
	if _, err = io.Copy(&output, r); err == nil || output.Len() > output.Limit {
		t.Fatal("copy bypassed output bound", err, output.Len())
	}
	var subprocess Buffer
	subprocess.Limit = 128
	cmd := exec.Command("bash", "-c", "printf '%01024d' 1")
	cmd.Stdout = &subprocess
	if err = Run(cmd, time.Second, time.Second); err == nil || subprocess.Len() > subprocess.Limit {
		t.Fatal("subprocess bypassed output bound", err, subprocess.Len())
	}
}
