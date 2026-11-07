#!/usr/bin/env python3
"""Build a deterministic offline zipapp from the explicitly selected checkout."""

import hashlib
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import zipfile

COMMIT = "fbe71f9f6edd108beceb01d568a0b17b5a6eb85c"


def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: build-led-artifact.py PINNED_CHECKOUT OUTPUT.pyz")
    source = pathlib.Path(sys.argv[1]).resolve(strict=True)
    target = pathlib.Path(sys.argv[2]).absolute()
    env = dict(os.environ, GIT_OPTIONAL_LOCKS="0")
    for args, expected in ((["rev-parse", "HEAD"], COMMIT), (["status", "--porcelain"], "")):
        actual = subprocess.check_output(["git", "-C", str(source), *args], env=env, text=True).strip()
        if actual != expected:
            raise SystemExit("LED source must be a clean checkout at the pinned commit")
    files = {}
    for path in sorted((source / "src" / "pistatus").glob("*.py")):
        if path.is_symlink() or not path.is_file():
            raise SystemExit("LED source contains an unexpected file type")
        files["pistatus/" + path.name] = path.read_bytes()
    if not files or len(files) > 48 or any(len(body) > 512 * 1024 for body in files.values()):
        raise SystemExit("LED source file set exceeds bounds")
    repository = pathlib.Path(__file__).resolve().parent.parent
    files["__main__.py"] = (repository / "internal/notification/ledartifact/launcher.py").read_bytes()
    files["pistatus/deploy_removal.py"] = (repository / "internal/notification/ledartifact/removal_guard.py").read_bytes()
    files["LICENSE"] = (source / "LICENSE").read_bytes()
    metadata = {
        "version": 1,
        "daemon_version": "0.1.0",
        "source_commit": COMMIT,
        "restore_guard": 1,
        "removal_guard": 1,
        "files": {name: hashlib.sha256(body).hexdigest() for name, body in files.items()},
    }
    files["deploy-led-artifact.json"] = (json.dumps(metadata, sort_keys=True, separators=(",", ":")) + "\n").encode()
    target.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=target.name + ".", dir=target.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as output:
            with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
                for name, body in sorted(files.items()):
                    info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                    info.create_system = 3
                    info.external_attr = 0o100644 << 16
                    info.compress_type = zipfile.ZIP_DEFLATED
                    archive.writestr(info, body, compresslevel=9)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, target)
        descriptor = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        temporary.unlink(missing_ok=True)
    print(json.dumps({"file": str(target), "sha256": hashlib.sha256(target.read_bytes()).hexdigest()}))


if __name__ == "__main__":
    main()
