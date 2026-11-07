package workerctl

import (
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/config"
)

type ResolvedSignalPaths struct {
	Account       string `json:"account"`
	Inbox         string `json:"inbox"`
	Service       string `json:"service"`
	Socket        string `json:"socket"`
	Control       string `json:"control"`
	Executable    string `json:"executable"`
	AccountFile   string `json:"account_file"`
	RecipientFile string `json:"recipient_file"`
}

// ResolveSignalPaths keeps account databases, inbox history and service artifacts
// outside gateway state. Custom socket roots must remain exclusively service-owned.
func ResolveSignalPaths(p config.ProviderDeclaration, paths ResolvedPaths, home, cliHash string) (ResolvedSignalPaths, error) {
	var r ResolvedSignalPaths
	for _, path := range []string{home, paths.Config, paths.Data, paths.State, paths.Installation, paths.Control, paths.Runtime, paths.Credentials, paths.UnitDirectory} {
		if !config.WorkerAbsolutePath(path) {
			return r, fmt.Errorf("Signal setup requires resolved absolute target paths")
		}
	}
	if !validDigest(cliHash) {
		return r, fmt.Errorf("supported Signal executable digest required")
	}
	r.Account = filepath.Join(filepath.Dir(paths.State), "signal-account")
	r.Inbox = filepath.Join(filepath.Dir(paths.State), "signal-inbox")
	r.Service = filepath.Join(paths.Data, "signal", "service")
	r.Control = filepath.Join(paths.Control, "signal")
	r.Executable = filepath.Join(paths.Data, "signal", "artifacts", cliHash, "signal-cli")
	var err error
	if p.Data != "" {
		r.Account, err = ResolveTargetPath(p.Data, home)
		if err != nil {
			return r, err
		}
	}
	if p.Executable != "" {
		if p.ExecutableSHA256 != cliHash {
			return r, fmt.Errorf("Signal executable declaration differs from selected artifact")
		}
		r.Executable, err = ResolveTargetPath(p.Executable, home)
		if err != nil {
			return r, err
		}
	}
	if p.Socket != "" {
		r.Socket, err = ResolveTargetPath(p.Socket, home)
		if err != nil {
			return r, err
		}
		r.Service = filepath.Dir(r.Socket)
	} else {
		r.Socket = filepath.Join(r.Service, "api.sock")
	}
	if len(r.Socket) > 100 {
		return r, fmt.Errorf("Signal service socket path too long; set providers.signal.socket")
	}
	resolve := func(target, input, name string) (string, error) {
		if target != "" && input != "" {
			return "", fmt.Errorf("ambiguous Signal credential reference")
		}
		if target != "" {
			return ResolveTargetPath(target, home)
		}
		if input != "" {
			return filepath.Join(paths.Credentials, "notification-worker", "signal-"+name), nil
		}
		return "", fmt.Errorf("Signal %s credential reference required", name)
	}
	r.AccountFile, err = resolve(p.AccountFile, p.AccountInput, "account")
	if err != nil {
		return r, err
	}
	r.RecipientFile, err = resolve(p.RecipientFile, p.RecipientInput, "recipient")
	if err != nil {
		return r, err
	}
	if r.AccountFile == r.RecipientFile {
		return r, fmt.Errorf("Signal credential references must be distinct")
	}
	roots := []string{r.Account, r.Inbox, r.Service, r.Control}
	for i, a := range roots {
		for _, b := range roots[:i] {
			if within(a, b) || within(b, a) {
				return r, fmt.Errorf("Signal state and service roots overlap")
			}
		}
		for _, b := range []string{paths.Config, paths.State, paths.Installation, paths.UnitDirectory, paths.Credentials} {
			if within(a, b) || within(b, a) {
				return r, fmt.Errorf("Signal root overlaps gateway authority")
			}
		}
	}
	for _, input := range []string{r.Executable, r.AccountFile, r.RecipientFile} {
		for _, root := range append(roots, paths.State, paths.Installation, paths.UnitDirectory, paths.Control) {
			if within(input, root) {
				return r, fmt.Errorf("Signal input overlaps managed state or service")
			}
		}
		if input == paths.Runtime {
			return r, fmt.Errorf("Signal input overlaps gateway runtime")
		}
	}
	if r.Executable == r.AccountFile || r.Executable == r.RecipientFile {
		return r, fmt.Errorf("Signal executable overlaps private credentials")
	}
	for _, denied := range paths.DeniedPaths {
		for _, root := range []string{r.Account, r.Inbox, r.Service} {
			if within(root, denied) || within(denied, root) {
				return r, fmt.Errorf("Signal root conflicts with excluded path")
			}
		}
	}
	return r, nil
}
