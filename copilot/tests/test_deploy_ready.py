"""Deploy-readiness pieces that need no cloud: disabled k8s actions and the bootstrap command."""

from __future__ import annotations

import pytest

from copilot import bootstrap
from copilot.actions.k8s import ActionError, DisabledK8sActions


def test_disabled_actions_refuse_with_a_fixed_message() -> None:
    api = DisabledK8sActions()
    with pytest.raises(ActionError, match="disabled"):
        api.restart_pods("ride-matching", "batcherd")
    with pytest.raises(ActionError, match="disabled"):
        api.scale("ride-matching", "batcherd", 3)


def test_bootstrap_requires_the_owner_dsn(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.delenv("COPILOT_OWNER_DSN", raising=False)
    assert bootstrap.main([]) == 2
    assert "COPILOT_OWNER_DSN" in capsys.readouterr().err


def test_bootstrap_default_root_is_the_repo_root_with_docs() -> None:
    assert (bootstrap.REPO_ROOT / "docs").is_dir()
