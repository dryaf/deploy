package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
)

type PathObservation struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Safe   bool   `json:"safe"`
	Kind   string `json:"kind,omitempty"`
}

type TargetStatus struct {
	Paths       []PathObservation `json:"paths"`
	LoadState   string            `json:"load_state"`
	ActiveState string            `json:"active_state"`
}

type StatusInspector interface {
	Status(context.Context, config.ServerConfig, string, ResolvedPaths) (TargetStatus, error)
}

const statusScript = `import os,sys,json,stat,subprocess
request=json.loads(sys.stdin.buffer.read(16385))
if request.get('version')!=1 or request.get('worker') not in ('backup-worker','notification-worker'): raise ValueError('invalid inspection request')
uid=os.getuid()
def observe(name,value):
 result={'name':name,'exists':False,'safe':True}
 if not value.startswith('/') or os.path.normpath(value)!=value or value=='/': raise ValueError('invalid path')
 fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
 try:
  parts=value.lstrip('/').split('/')
  for i,part in enumerate(parts):
   try:
    flags=os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK
    if i<len(parts)-1: flags|=os.O_DIRECTORY
    child=os.open(part,flags,dir_fd=fd)
   except FileNotFoundError: return result
   except OSError:
    result['exists']=True;result['safe']=False;return result
   os.close(fd);fd=child
  s=os.fstat(fd)
  result['exists']=True
  result['kind']='directory' if stat.S_ISDIR(s.st_mode) else 'file' if stat.S_ISREG(s.st_mode) else 'other'
  result['safe']=s.st_uid==uid and not s.st_mode&0o022 and result['kind']!='other'
  if result['kind']=='file': result['safe']=result['safe'] and s.st_nlink==1
  return result
 finally: os.close(fd)
paths=request.get('paths',{})
if len(paths)>16: raise ValueError('too many paths')
result={'paths':[observe(k,v) for k,v in sorted(paths.items())],'load_state':'unknown','active_state':'unknown'}
try:
 p=subprocess.run(['systemctl','--user','show',{'backup-worker':'deploy-backup.service','notification-worker':'deploy-notification-worker.service'}[request['worker']],'--property=LoadState','--property=ActiveState'],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5,check=False)
 properties=dict(line.split('=',1) for line in p.stdout.decode('utf-8','replace').splitlines() if '=' in line)
 for prop,key in [('LoadState','load_state'),('ActiveState','active_state')]:
  value=properties.get(prop,'unknown')
  if value in ('loaded','not-found','error','masked','active','inactive','activating','deactivating','failed','unknown'): result[key]=value
except (OSError,subprocess.TimeoutExpired): pass
print(json.dumps(result))
`

func statusRequest(kind string, p ResolvedPaths) []byte {
	paths := map[string]string{"config": p.Config, "state": p.State, "runtime": p.Runtime, "control": p.Control}
	if kind == "notification-worker" {
		paths["installation"] = p.Installation
		paths["manifest"] = path.Join(p.Installation, "installation.json")
		paths["identity"] = path.Join(p.State, "identity.json")
		if p.Data != "" {
			paths["led_prerequisites"] = path.Join(p.Data, "led-prerequisites")
		}
	} else {
		paths["archive"] = p.Archive
		paths["manifest"] = p.Runtime
		paths["setup"] = p.backupSetup
		if paths["setup"] == "" {
			paths["setup"] = path.Join(path.Dir(p.State), "setup.json")
		}
	}
	body, _ := json.Marshal(struct {
		Version int               `json:"version"`
		Worker  string            `json:"worker"`
		Paths   map[string]string `json:"paths"`
	}{1, kind, paths})
	return body
}

func (SSHInspector) Status(ctx context.Context, s config.ServerConfig, kind string, p ResolvedPaths) (TargetStatus, error) {
	var result TargetStatus
	cmd := transport.WorkerAdminShell(s, "python3 -I -c "+transport.ShellQuote(statusScript))
	cmd.Stdin = bytes.NewReader(statusRequest(kind, p))
	var stdout, stderr process.Buffer
	stdout.Limit = 16384
	stderr.Limit = 4096
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := process.RunContext(ctx, cmd, 20*time.Second, 2*time.Second); err != nil {
		return result, fmt.Errorf("read-only target status unavailable")
	}
	if err := safefs.DecodeJSON(stdout.Bytes(), &result, 16384); err != nil {
		return result, fmt.Errorf("invalid target status response")
	}
	return result, nil
}

func ApplyStatus(p Plan, status TargetStatus) Plan {
	p.Actions = nil
	p.Observed = &status
	manifest, state, unsafe, anything, setup := false, false, false, false, false
	seen := map[string]bool{}
	for _, v := range status.Paths {
		if seen[v.Name] {
			unsafe = true
		}
		seen[v.Name] = true
		anything = anything || v.Exists
		unsafe = unsafe || !v.Safe
		if v.Name == "manifest" {
			manifest = v.Exists
		}
		if v.Name == "state" {
			state = v.Exists
		}
		if v.Name == "setup" {
			setup = v.Exists
		}
	}
	if len(status.Paths) == 0 || !seen["manifest"] || !seen["state"] {
		unsafe = true
	}
	switch {
	case unsafe:
		p.State = "recovery_required"
		p.Code = "unsafe_path"
		p.Next = "inspect ownership and path conflicts before changing the installation"
	case !anything && status.LoadState != "loaded":
		p.State = "not_installed"
		p.Code = "not_installed"
		p.Next = "complete configuration before installation"
	case state && !manifest && !(p.Worker == "backup-worker" && setup):
		p.State = "recovery_required"
		p.Code = "missing_manifest"
		p.Next = "inspect existing state; do not initialize a replacement database"
	default:
		p.State = "needs_action"
		p.Code = "admission_unverified"
		p.Next = "inspect the existing domain installation and qualification records before adopting it"
	}
	p.Unresolved = []string{"canonical installation ownership and qualification are not established by filesystem presence or service activity"}
	sort.Slice(p.Observed.Paths, func(i, j int) bool { return p.Observed.Paths[i].Name < p.Observed.Paths[j].Name })
	return p
}
