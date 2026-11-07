"""Pinned managed-daemon extension for atomic, root-authorized retirement."""

import json
import os
from pathlib import Path
import re
import stat
import tempfile

from . import protocol
from .models import StatusError
from .persistence import strict_json


def _path(application):
    if not application.config.persistence_enabled:
        raise StatusError("storage_error", "Managed removal requires persistence")
    return Path(application.config.persistence_path).parent / "removal.json"


def _read(application):
    path = _path(application)
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    with os.fdopen(descriptor, "rb") as source:
        info = os.fstat(source.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid()
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
            raise ValueError("Unsafe managed removal fence")
        raw = source.read(4097)
    if len(raw) > 4096:
        raise ValueError("Managed removal fence exceeds bound")
    record = strict_json(raw.decode("utf-8"))
    if (not isinstance(record, dict) or set(record) != {"version", "plan_hash"}
            or type(record["version"]) is not int or record["version"] != 1
            or not isinstance(record["plan_hash"], str)
            or re.fullmatch(r"[a-f0-9]{64}", record["plan_hash"]) is None):
        raise ValueError("Invalid managed removal fence")
    return record


def _publish(application, record):
    path = _path(application)
    parent = path.parent.stat()
    if parent.st_uid != os.geteuid() or stat.S_IMODE(parent.st_mode) != 0o700:
        raise ValueError("Unsafe managed removal directory")
    descriptor, temporary = tempfile.mkstemp(prefix=".removal-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as output:
            os.fchmod(output.fileno(), 0o600)
            output.write(json.dumps(record, separators=(",", ":")).encode("ascii"))
            output.flush()
            os.fsync(output.fileno())
        # One daemon holds the persistence lease; never replace another intent.
        os.link(temporary, path, follow_symlinks=False)
        os.unlink(temporary)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def install(application_type):
    original_request = application_type.request
    original_status = application_type.status

    async def request(self, request, uid):
        operation = request.get("op")
        if operation == "deploy.prepare-removal":
            protocol.validate_envelope(request, {"plan_hash"}, {"plan_hash"})
            if uid != 0:
                raise StatusError("permission_denied", "Managed removal requires root")
            plan_hash = request["plan_hash"]
            if not isinstance(plan_hash, str) or re.fullmatch(r"[a-f0-9]{64}", plan_hash) is None:
                raise StatusError("invalid_request", "Exact managed removal plan required")
            async with self.lock:
                try:
                    retained = _read(self)
                    record = {"version": 1, "plan_hash": plan_hash}
                    if retained is not None and retained != record:
                        raise ValueError("Managed removal plan changed")
                    if self.registry.storage_error or self.registry.active():
                        raise StatusError("states_remaining", "Clear retained LED obligations before removal")
                    if retained is None:
                        _publish(self, record)
                    return record
                except (OSError, ValueError, TypeError) as error:
                    raise StatusError("storage_error", "Managed removal fence unavailable") from error
        # The upstream coroutine acquires the request lock; the guarded registry
        # methods below check the fence inside that same critical section.
        return await original_request(self, request, uid)

    def status(self):
        result = original_status(self)
        try:
            removing = _read(self) is not None
        except (OSError, ValueError, TypeError):
            removing = True
        if removing:
            result["degraded"] = True
            result["removal_pending"] = True
        return result

    original_init = application_type.__init__

    def initialize(self, *args, **kwargs):
        original_init(self, *args, **kwargs)
        # Application.request serializes these synchronous methods with its
        # asyncio lock, shared with prepare-removal. No check/stop race remains.
        for name in ("set", "clear"):
            original = getattr(self.registry, name)

            def guarded(*args, _original=original, **kwargs):
                try:
                    if _read(self) is not None:
                        raise StatusError("removal_pending", "Managed daemon is fenced for removal")
                except (OSError, ValueError, TypeError) as error:
                    raise StatusError("storage_error", "Managed removal fence unavailable") from error
                return _original(*args, **kwargs)

            setattr(self.registry, name, guarded)

    application_type.__init__ = initialize
    application_type.request = request
    application_type.status = status
