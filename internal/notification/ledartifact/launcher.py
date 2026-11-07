"""Deploy's pinned daemon entry point; only standard-library dependencies."""

import sys

if sys.version_info < (3, 11):
    raise SystemExit("pi-status-led requires Python 3.11 or later")

from pistatus.daemon import main
from pistatus.sysfs import SysfsLEDDriver
from pistatus.api import Application
from pistatus.deploy_removal import install as install_removal_guard

install_removal_guard(Application)


# A clean shutdown must not overwrite an administrator's intervening hardware
# change. The upstream driver retains the last values it wrote; compare those
# immediately before its original restoration routine.
_restore_original = SysfsLEDDriver.restore_original


def _restore_owned(self):
    if self.original is not None:
        current = self.inspect()
        if current.trigger != self._trigger:
            raise OSError("LED trigger changed outside the managed daemon; restoration deferred")
        if self._trigger == "timer":
            delays = tuple(int((self.path / name).read_text().strip()) for name in ("delay_on", "delay_off"))
            if delays != self._timer:
                raise OSError("LED timer changed outside the managed daemon; restoration deferred")
        elif self._trigger == "none":
            if int((self.path / "brightness").read_text().strip()) != self._brightness:
                raise OSError("LED brightness changed outside the managed daemon; restoration deferred")
        else:
            raise OSError("LED output ownership unavailable; restoration deferred")
    return _restore_original(self)


SysfsLEDDriver.restore_original = _restore_owned
raise SystemExit(main())
