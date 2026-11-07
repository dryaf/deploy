package workerctl

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
)

// Inspection uses the existing SSH trust policy and performs no filesystem writes.
// No declaration or secret value is interpolated into remote shell code.
const inspectScript = `import os,pwd,platform,json,shutil,subprocess,glob
u=pwd.getpwuid(os.getuid())
def probe(args):
 try:
  p=subprocess.run(args,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5,check=False)
  return p.returncode==0,p.stdout.decode('utf-8','replace').strip()
 except (OSError,subprocess.TimeoutExpired): return False,''
def read_model():
 try:
  with open('/proc/device-tree/model','rb') as f: return f.read(256).decode('utf-8','replace').strip('\x00\n')
 except OSError: return ''
f={'os':platform.system().lower(),'arch':{'x86_64':'amd64','aarch64':'arm64'}.get(platform.machine(),platform.machine()),'uid':u.pw_uid,'user':u.pw_name,'home':u.pw_dir,'config_home':os.environ.get('XDG_CONFIG_HOME',''),'state_home':os.environ.get('XDG_STATE_HOME',''),'data_home':os.environ.get('XDG_DATA_HOME',''),'runtime_dir':os.environ.get('XDG_RUNTIME_DIR',''),'tools':{n:shutil.which(n)!=None for n in ['systemctl','loginctl','busctl','podman','ssh','ssh-keygen','bwrap','python3']},'user_manager':probe(['systemctl','--user','show-environment'])[0],'linger':probe(['loginctl','show-user',str(u.pw_uid),'--property=Linger','--value'])[1]=='yes','board':read_model(),'leds':sorted(os.path.basename(p) for p in glob.glob('/sys/class/leds/*')),'led_paths':{os.path.basename(p):os.path.realpath(p) for p in glob.glob('/sys/class/leds/*')}}
print(json.dumps(f))
`

type Inspector interface {
	Inspect(context.Context, config.ServerConfig) (TargetFacts, error)
}
type SSHInspector struct{}

func (SSHInspector) Inspect(ctx context.Context, s config.ServerConfig) (TargetFacts, error) {
	var facts TargetFacts
	cmd := transport.WorkerAdminShell(s, "python3 -I -c "+transport.ShellQuote(inspectScript))
	if err := ctx.Err(); err != nil {
		return facts, err
	}
	var stdout, stderr process.Buffer
	stdout.Limit = 32768
	stderr.Limit = 4096
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := process.RunContext(ctx, cmd, 30*time.Second, 2*time.Second)
	raw := stdout.Bytes()
	if err != nil {
		return facts, fmt.Errorf("target inspection failed; verify SSH trust, login and python3")
	}
	if err = ctx.Err(); err != nil {
		return facts, err
	}
	if err = safefs.DecodeJSON(raw, &facts, 32768); err != nil {
		return facts, fmt.Errorf("invalid target inspection response")
	}
	if facts.User != s.User {
		return facts, fmt.Errorf("SSH target user does not match inventory")
	}
	return facts, nil
}
