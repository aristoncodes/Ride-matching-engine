"""Offline tests for eval/run.py injector handling."""

from __future__ import annotations

import importlib.util
import subprocess
import sys
from pathlib import Path

EVAL = Path(__file__).resolve().parent.parent / "eval"
sys.path.insert(0, str(EVAL))
spec = importlib.util.spec_from_file_location("eval_run", EVAL / "run.py")
er = importlib.util.module_from_spec(spec)
spec.loader.exec_module(er)

KEY = "sk-test-SECRET-KEY-VALUE"


def test_api_key_forwarded_via_env_not_argv(monkeypatch, capsys):
    monkeypatch.setenv("RIDEMATCH_API_KEY", KEY)
    captured = {}

    def fake_popen(args, **kw):
        captured["args"], captured["env"] = args, kw["env"]
        return object()

    monkeypatch.setattr(er.subprocess, "Popen", fake_popen)
    er.start_injector("traffic_burst")
    assert captured["env"]["RIDEMATCH_API_KEY"] == KEY
    assert all(KEY not in str(a) for a in captured["args"])
    out = capsys.readouterr()
    assert KEY not in out.out + out.err


def test_api_key_absent_when_not_set(monkeypatch):
    monkeypatch.delenv("RIDEMATCH_API_KEY", raising=False)
    captured = {}
    monkeypatch.setattr(
        er.subprocess, "Popen", lambda a, **kw: captured.update(env=kw["env"]) or object()
    )
    er.start_injector("traffic_burst")
    assert "RIDEMATCH_API_KEY" not in captured["env"]


class FakeProc:
    def __init__(self, exits_on_term: bool):
        self.exits_on_term = exits_on_term
        self.calls: list[str] = []

    def terminate(self):
        self.calls.append("terminate")

    def kill(self):
        self.calls.append("kill")

    def wait(self, timeout=None):
        if "kill" in self.calls or (self.exits_on_term and "terminate" in self.calls):
            return -15
        raise subprocess.TimeoutExpired("x", timeout)


def test_stop_injector_sigterm_first_no_kill_when_it_exits():
    p = FakeProc(exits_on_term=True)
    er.stop_injector(p)
    assert p.calls == ["terminate"]


def test_stop_injector_kills_only_after_grace():
    p = FakeProc(exits_on_term=False)
    er.stop_injector(p)
    assert p.calls == ["terminate", "kill"]
