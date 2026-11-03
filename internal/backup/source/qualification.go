package source

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

func QualifyRestrictedSource(source WorkerSource, policy config.WorkerTimeouts) error {
	var capabilities struct {
		Protocol int    `json:"protocol"`
		SourceID string `json:"source_id"`
		MaxBytes int64  `json:"max_snapshot_bytes"`
		Format   string `json:"format"`
	}
	if err := QueryRestrictedSource(context.Background(), source, policy, "capabilities", &capabilities); err != nil {
		return err
	}
	if capabilities.Protocol != 1 || capabilities.SourceID != source.SourceID || capabilities.Format != "sqlite-delete-v1" {
		return fmt.Errorf("source helper capabilities/identity mismatch")
	}
	key, err := os.ReadFile(source.KeyFile)
	if err != nil {
		return fmt.Errorf("worker identity unavailable")
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("worker identity invalid")
	}
	callback, err := knownhosts.New(source.KnownHosts)
	if err != nil {
		return fmt.Errorf("pinned source host key invalid")
	}
	address := net.JoinHostPort(source.Host, strconv.Itoa(source.Port))
	connection, err := net.DialTimeout("tcp", address, config.Budget(policy.Connect, "10s"))
	if err != nil {
		return fmt.Errorf("restriction test connection unavailable")
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	cc, channels, requests, err := ssh.NewClientConn(connection, address, &ssh.ClientConfig{User: source.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback})
	if err != nil {
		return fmt.Errorf("restriction test authentication failed")
	}
	client := ssh.NewClient(cc, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	if err = session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
		session.Close()
		return fmt.Errorf("source key unexpectedly permits PTY allocation")
	}
	if err = agent.RequestAgentForwarding(session); err == nil {
		session.Close()
		return fmt.Errorf("source key unexpectedly permits agent forwarding")
	}
	accepted, err := session.SendRequest("x11-req", true, ssh.Marshal(struct {
		Single   bool
		Protocol string
		Cookie   string
		Screen   uint32
	}{false, "MIT-MAGIC-COOKIE-1", "00000000000000000000000000000000", 0}))
	session.Close()
	if err != nil || accepted {
		return fmt.Errorf("X11 forwarding denial could not be established")
	}
	listener, err := client.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		listener.Close()
		return fmt.Errorf("source key unexpectedly permits remote forwarding")
	}
	forwarded, err := client.Dial("tcp", "127.0.0.1:1")
	if err == nil {
		forwarded.Close()
		return fmt.Errorf("source key unexpectedly permits direct forwarding")
	}
	denial, ok := err.(*ssh.OpenChannelError)
	if !ok || denial.Reason != ssh.Prohibited {
		return fmt.Errorf("direct forwarding denial could not be established")
	}
	for _, command := range []string{"printf DEPLOY_UNRESTRICTED_EXEC", "scp -f /etc/passwd", "sftp"} {
		session, err := client.NewSession()
		if err != nil {
			return err
		}
		var out, stderr process.Buffer
		out.Limit = 4096
		stderr.Limit = 4096
		session.Stdout = &out
		session.Stderr = &stderr
		err = session.Run(command)
		session.Close()
		if err == nil || bytes.Contains(out.Bytes(), []byte("DEPLOY_UNRESTRICTED_EXEC")) {
			return fmt.Errorf("forced-command denial could not be established")
		}
	}
	return nil
}
