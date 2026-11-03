# Read-only inventory probe. The CLI supplies this program over stdin; nothing is
# installed on the inspected host. Results contain no environment or secret data.
import hashlib
import json
import itertools
import os
import re
import resource
import selectors
import signal
import stat
import subprocess
import sys
import time

if sys.platform == "linux":
    resource.setrlimit(resource.RLIMIT_AS, (256 * 1024 * 1024, 256 * 1024 * 1024))
LIMIT = 1024 * 1024
END = time.monotonic() + 25
result = {"schema": 1, "uid": os.getuid(), "files": [], "units": {},
          "containers": [], "diagnostics": [], "complete": True}


def diagnostic(message):
    result["complete"] = False
    result["diagnostics"].append(message)


def strict_json(text):
    def unique(pairs):
        value = {}
        for key, item in pairs:
            if key in value:
                raise ValueError("duplicate JSON key")
            value[key] = item
        return value
    return json.loads(text, object_pairs_hook=unique)


def read_file(name, limit=LIMIT):
    fd = os.open(name, os.O_RDONLY | os.O_NONBLOCK)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise ValueError("not a regular file")
        data = bytearray()
        while len(data) <= limit:
            chunk = os.read(fd, min(65536, limit + 1 - len(data)))
            if not chunk:
                return data.decode("utf-8", errors="strict")
            data.extend(chunk)
        raise ValueError("size limit exceeded")
    finally:
        os.close(fd)


def command(args):
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            stdin=subprocess.DEVNULL, start_new_session=True)
    try:
        output = bytearray()
        with selectors.DefaultSelector() as selector:
            selector.register(proc.stdout, selectors.EVENT_READ)
            while True:
                remaining = min(5, END - time.monotonic())
                if remaining <= 0 or not selector.select(remaining):
                    raise ValueError("command timeout")
                chunk = os.read(proc.stdout.fileno(), 65536)
                if not chunk:
                    break
                output.extend(chunk)
                if len(output) > 4 * LIMIT:
                    raise ValueError("command output limit")
        if proc.wait(timeout=max(.01, min(1, END-time.monotonic()))) != 0:
            raise ValueError("command failed")
        return output.decode("utf-8", errors="strict")
    finally:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait()
        proc.stdout.close()


uid = os.getuid()
home = os.path.expanduser("~")
runtime = os.environ.get("XDG_RUNTIME_DIR", "/run/user/" + str(uid))
config = os.environ.get("XDG_CONFIG_HOME", os.path.join(home, ".config"))
if uid == 0:
    roots = ["/run/containers/systemd", "/etc/containers/systemd", "/usr/share/containers/systemd"]
    systemctl = ["systemctl"]
else:
    roots = [os.path.join(runtime, "containers/systemd"), os.path.join(config, "containers/systemd"),
             "/etc/containers/systemd/users/" + str(uid), "/etc/containers/systemd/users"]
    systemctl = ["systemctl", "--user"]
# A generator search override cannot be inferred from the SSH environment.
if os.environ.get("QUADLET_UNIT_DIRS"):
    diagnostic("QUADLET_UNIT_DIRS is set; generator search paths require reconciliation")
    roots = os.environ["QUADLET_UNIT_DIRS"].split(":")
seen_paths = set()
visited = set()
file_bytes = 0


def walk(root, directory, precedence, depth=0):
    global file_bytes
    if depth > 16 or len(visited) >= 512 or time.monotonic() >= END:
        diagnostic("Quadlet search limit reached")
        return
    try:
        real = os.path.realpath(directory)
        if real in visited:
            return
        visited.add(real)
        with os.scandir(directory) as listing:
            entries = sorted(itertools.islice(listing, 4097), key=lambda entry: entry.name)
        if len(entries) > 4096:
            diagnostic("Quadlet directory entry count exceeds limit")
            return
    except FileNotFoundError:
        return
    except OSError:
        diagnostic("Quadlet search directory inaccessible: " + directory)
        return
    for entry in entries:
        # The shared user directory must not enumerate other accounts.
        if root == "/etc/containers/systemd/users" and entry.name.isdecimal():
            continue
        if entry.is_dir(follow_symlinks=True) and not entry.name.endswith(".d"):
            walk(root, entry.path, precedence, depth + 1)
        elif entry.name.endswith(".container") and entry.path not in seen_paths:
            if len(result["files"]) >= 512 or file_bytes >= 8 * LIMIT:
                diagnostic("Quadlet file count limit reached")
                return
            seen_paths.add(entry.path)
            item = {"path": entry.path, "name": entry.name, "precedence": precedence, "resolved_path": os.path.realpath(entry.path),
                    "masked": os.path.realpath(entry.path) == "/dev/null"}
            try:
                if not item["masked"]:
                    item["content"] = read_file(entry.path)
                    file_bytes += len(item["content"].encode("utf-8"))
                    if file_bytes > 8 * LIMIT:
                        raise ValueError("aggregate file limit")
                dropins = ["container.d", entry.name + ".d"]
                stem = entry.name[:-10]
                for index, char in enumerate(stem):
                    if char == "-":
                        dropins.append(stem[:index+1] + ".container.d")
                item["dropins"] = any(os.path.isdir(os.path.join(base, suffix))
                                      for base in roots + [os.path.dirname(entry.path)] for suffix in dropins)
            except (OSError, ValueError, UnicodeError):
                item.pop("content", None)
                item["error"] = "Quadlet unreadable, nonregular or oversized"
                diagnostic(item["error"] + ": " + entry.path)
            result["files"].append(item)


for precedence, root in enumerate(roots):
    walk(root, root, precedence)

unit_names = {item["name"][:-10] + ".service" for item in result["files"]}
try:
    unit_files = command(systemctl + ["list-unit-files", "--type=service", "--no-legend", "--no-pager"])
    for line in unit_files.splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[1] == "generated":
            unit_names.add(fields[0])
except (OSError, ValueError, subprocess.SubprocessError):
    diagnostic("systemd generated-unit inventory unavailable")
if len(unit_names) > 512:
    diagnostic("systemd unit count exceeds limit")
for name in sorted(unit_names)[:512]:
    try:
        text = command(systemctl + ["show", "--no-pager", "--property=Id,LoadState,ActiveState,SubState,SourcePath,FragmentPath,DropInPaths,ExecStart,Result", "--", name])
        properties = dict(line.split("=", 1) for line in text.splitlines() if "=" in line)
        properties["ResolvedSourcePath"] = os.path.realpath(properties["SourcePath"]) if properties.get("SourcePath") else ""
        start = properties.pop("ExecStart", "")
        # Generated Quadlet --cidfile identifies a container without guessing its name.
        match = re.search(r"--cidfile(?:=|\s+)([^\s;]+)", start)
        if match:
            cidfile = match.group(1).strip('"')
            if os.path.realpath(cidfile).startswith(os.path.realpath(runtime) + "/"):
                try:
                    cid = read_file(cidfile, 128).strip()
                    if re.fullmatch(r"[0-9a-f]{64}", cid):
                        properties["ContainerID"] = cid
                except (OSError, ValueError):
                    pass
        result["units"][name] = properties
    except (OSError, ValueError, subprocess.SubprocessError):
        diagnostic("systemd state unavailable: " + name)

try:
    ids = command(["podman", "ps", "--all", "--no-trunc", "--format", "{{.ID}}"])
    ids = ids.splitlines()
    if len(ids) > 512:
        diagnostic("Podman container count exceeds limit")
        ids = ids[:512]
    for cid in ids:
        if not re.fullmatch(r"[0-9a-f]{64}", cid):
            raise ValueError("invalid container ID")
        try:
            inspected = json.loads(command(["podman", "inspect", "--type=container", cid]))[0]
            labels = inspected.get("Config", {}).get("Labels") or {}
            result["containers"].append({"id": inspected["Id"], "name": inspected.get("Name", ""),
                "state": inspected.get("State", {}).get("Status", "unknown"),
                "labels": {k: v for k, v in labels.items() if k.startswith("io.deploy.")}})
        except (OSError, ValueError, KeyError, IndexError, subprocess.SubprocessError):
            diagnostic("Podman inspect unavailable: " + cid)
except (OSError, ValueError, subprocess.SubprocessError):
    diagnostic("Podman inventory unavailable")

# Management evidence is independent of container names and marker claims.
locks = os.path.join(home, ".local/state/deploy/locks")
for item in result["files"]:
    if time.monotonic() >= END:
        diagnostic("ownership evidence deadline exceeded")
        break
    name = item["name"][:-10]
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", name) or ".." in name:
        continue
    try:
        owner = strict_json(read_file(os.path.join(locks, hashlib.sha256(name.encode()).hexdigest()+".owner.json"), 16384))
        target = owner.get("target", "")
        mode = owner.get("mode", "image")
        if not os.path.isabs(target) or target == "/" or mode not in ("image", "binary"):
            continue
        target_owner = strict_json(read_file(os.path.join(locks, "target-"+hashlib.sha256(target.encode()).hexdigest()+".owner.json"), 16384))
        state = os.path.join(target, ".deploy", mode)
        current = strict_json(read_file(os.path.join(state,"current.json"), 16384))
        release = current.get("current", "")
        if not re.fullmatch(r"[a-zA-Z0-9_-]{1,200}", release):
            continue
        evidence = {"owner": {k: owner.get(k) for k in ("schema", "service", "target", "mode")},
                    "target_owner": {k: target_owner.get(k) for k in ("schema", "service", "target", "mode")},
                    "current": {k: current.get(k) for k in ("schema", "service", "target", "current")}}
        candidate = read_file(os.path.join(state,"releases",release,"candidate.container"))
        evidence["candidate_sha256"] = hashlib.sha256(candidate.encode()).hexdigest()
        evidence["pending"] = any(os.path.lexists(os.path.join(target,".deploy",m,p))
                                   for m in ("image","binary","db") for p in ("pending.json","boot-pending.json"))
        item["evidence"] = evidence
    except (OSError, ValueError, TypeError, UnicodeError):
        pass

print(json.dumps(result, separators=(",", ":")))
