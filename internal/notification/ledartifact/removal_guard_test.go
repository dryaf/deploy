package ledartifact

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

func TestDaemonRemovalGuardSerializesClientsAndSurvivesRestart(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	file := filepath.Join(t.TempDir(), "guard.py")
	if err = os.WriteFile(file, removalGuard, 0600); err != nil {
		t.Fatal(err)
	}
	script := `import asyncio,importlib.util,json,os,pathlib,sys,tempfile,types
package=types.ModuleType('pistatus');package.__path__=[];sys.modules['pistatus']=package
protocol=types.ModuleType('pistatus.protocol')
def validate(request,allowed,required):
 assert set(request)=={'v','op'}|required and request['v']==1
protocol.validate_envelope=validate;sys.modules[protocol.__name__]=protocol
class StatusError(Exception):
 def __init__(self,code,message):self.code=code;super().__init__(message)
models=types.ModuleType('pistatus.models');models.StatusError=StatusError;sys.modules[models.__name__]=models
persistence=types.ModuleType('pistatus.persistence');persistence.strict_json=json.loads;sys.modules[persistence.__name__]=persistence
spec=importlib.util.spec_from_file_location('pistatus.deploy_removal',sys.argv[1]);guard=importlib.util.module_from_spec(spec);spec.loader.exec_module(guard)
class Registry:
 def __init__(self):self.states={};self.storage_error=False
 def active(self):return self.states
 def set(self,uid,value):self.states[(uid,value)]=value;return value
 def clear(self,uid,value):return self.states.pop((uid,value),None)
class Application:
 def __init__(self,root):
  self.config=types.SimpleNamespace(persistence_enabled=True,persistence_path=str(root/'states.json'))
  self.registry=Registry();self.lock=asyncio.Lock()
 def status(self):return {'degraded':False}
 async def request(self,request,uid):
  async with self.lock:
   if request['op']=='state.set':return self.registry.set(uid,request['id'])
   if request['op']=='state.clear':return self.registry.clear(uid,request['id'])
   return self.status()
guard.install(Application)
async def main(root):
 app=Application(root);plan='a'*64;retire={'v':1,'op':'deploy.prepare-removal','plan_hash':plan}
 async def refused(request,uid,code):
  try:await app.request(request,uid)
  except StatusError as error:assert error.code==code,(error.code,code)
  else:raise AssertionError('request accepted')
 await refused(retire,1000,'permission_denied');assert not (root/'removal.json').exists()
 # Both persistent faults and transient healthy leases are obligations. No
 # worker-specific ID prefix is permission to drop another client's state.
 await app.request({'op':'state.set','id':'foreign-fault'},1001)
 await refused(retire,0,'states_remaining');assert not (root/'removal.json').exists()
 assert app.registry.states=={(1001,'foreign-fault'):'foreign-fault'}
 await app.request({'op':'state.clear','id':'foreign-fault'},1001)
 app.registry.storage_error=True;await refused(retire,0,'states_remaining');app.registry.storage_error=False
 # Retirement wins the lock; a queued writer cannot enter after the empty
 # check. Its failure leaves the registry empty and the fence durable.
 outcomes=await asyncio.gather(app.request(retire,0),app.request({'op':'state.set','id':'racing-client'},1000),return_exceptions=True)
 assert outcomes[0]=={'version':1,'plan_hash':plan}
 assert isinstance(outcomes[1],StatusError) and outcomes[1].code=='removal_pending'
 assert not app.registry.states and app.status()['degraded']
 original=(root/'removal.json').read_bytes()
 assert await app.request(retire,0)==outcomes[0]
 assert (root/'removal.json').read_bytes()==original
 assert (root/'removal.json').stat().st_mode&0o777==0o600
 app=Application(root) # process lifetime replacement, same durable authority
 await refused({'op':'state.set','id':'after-restart'},1000,'removal_pending')
 await refused({'op':'state.clear','id':'foreign'},1000,'removal_pending')
 await refused(dict(retire,plan_hash='b'*64),0,'storage_error')
 assert await app.request(retire,0)==outcomes[0]
 (root/'removal.json').write_text('{}')
 await refused({'op':'state.set','id':'lost-authority'},1000,'storage_error')
 await refused(retire,0,'storage_error')
 assert app.status()['degraded']
with tempfile.TemporaryDirectory() as directory:asyncio.run(main(pathlib.Path(directory)))
`
	cmd := exec.Command(python, "-I", "-c", script, file)
	if out, err := process.LimitedOutput(cmd, 10*time.Second, 4096); err != nil {
		t.Fatal(err, string(out))
	}
}
