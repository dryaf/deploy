"""Exercise a trusted built archive against its real API and persistent registry.

Usage: python3 -I scripts/test-led-artifact.py PATH_TO_DAEMON.pyz
Uses synthetic peer UIDs through the in-process API; no root, socket, sysfs or
provider access. Root authorization at the socket is covered by the daemon API.
"""

import asyncio
import pathlib
import sys
import tempfile

if len(sys.argv) != 2:
    raise SystemExit("usage: test-led-artifact.py TRUSTED_DAEMON.pyz")
sys.path.insert(0, str(pathlib.Path(sys.argv[1]).resolve(strict=True)))
from pistatus.api import Application
from pistatus.config import from_dict
from pistatus.deploy_removal import install
from pistatus.models import StatusError
from pistatus.registry import Registry

install(Application)

async def exercise(directory):
    root = pathlib.Path(directory)
    config = from_dict({"hardware": {"driver": "mock"}, "api": {"socket_path": str(root / "api.sock")}, "persistence": {"enabled": True, "path": str(root / "states.json")}})
    registry = Registry(config)
    registry.restore()
    app = Application(config, registry, {})
    fence = {"v": 1, "op": "deploy.prepare-removal", "plan_hash": "a" * 64}
    state = {"id": "other-client-fault", "led": "green", "pattern": "off", "priority": 90, "ttl_seconds": None, "persistent": True, "message": "synthetic fixture"}
    async def rejected(request, uid, code):
        try:
            await app.request(request, uid)
        except StatusError as error:
            assert error.code == code, (error.code, code)
        else:
            raise AssertionError("request accepted")
    await app.request({"v": 1, "op": "state.set", "state": state}, 1000)
    await rejected(fence, 0, "states_remaining")
    assert len(registry.active()) == 1
    await app.request({"v": 1, "op": "state.clear", "id": state["id"]}, 1000)
    await rejected(fence, 1000, "permission_denied")
    assert (await app.request(fence, 0))["plan_hash"] == "a" * 64
    await rejected({"v": 1, "op": "state.set", "state": state}, 1000, "removal_pending")
    registry = Registry(config)
    registry.restore()
    app = Application(config, registry, {})
    await rejected({"v": 1, "op": "state.set", "state": state}, 1000, "removal_pending")
    assert (await app.request(fence, 0))["plan_hash"] == "a" * 64
    assert app.status()["removal_pending"]
    assert not registry.active()

with tempfile.TemporaryDirectory(prefix="deploy-led-fence-") as directory:
    asyncio.run(exercise(directory))
print("pinned archive: actual API/registry persistence and restart fence passed; no hardware or provider used")
