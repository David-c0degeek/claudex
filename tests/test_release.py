from __future__ import annotations

import json
import tempfile
import unittest
import zipfile
from pathlib import Path

from claudex.config import (
    CONFIG_SCHEMA_VERSION,
    LEGACY_BROAD_CLAUDE_TOOLS,
    Config,
)
from claudex.recovery import export_run
from claudex.state import STATE_SCHEMA_VERSION, RunState


class ConfigMigrationTests(unittest.TestCase):
    def test_legacy_default_tools_migrate_narrowly_with_rollback_backup(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            path = repo / ".claudex" / "config.json"
            path.parent.mkdir()
            original = {
                "lead": "codex",
                "claude_write_allowed_tools": LEGACY_BROAD_CLAUDE_TOOLS,
            }
            path.write_text(json.dumps(original), encoding="utf-8")
            cfg = Config.load(repo)
            self.assertEqual(CONFIG_SCHEMA_VERSION, cfg.config_schema_version)
            self.assertNotEqual(LEGACY_BROAD_CLAUDE_TOOLS, cfg.claude_write_allowed_tools)
            backup = path.with_name("config.v1.bak.json")
            self.assertEqual(original, json.loads(backup.read_text(encoding="utf-8")))
            migrated = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(CONFIG_SCHEMA_VERSION, migrated["config_schema_version"])

    def test_custom_broad_tools_and_future_config_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            path = repo / ".claudex" / "config.json"
            path.parent.mkdir()
            path.write_text(
                json.dumps({"claude_write_allowed_tools": "Read,Edit,Bash"}),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "broad"):
                Config.load(repo)
            path.write_text(
                json.dumps({"config_schema_version": CONFIG_SCHEMA_VERSION + 1}),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "newer than supported"):
                Config.load(repo)


class RecoveryExportTests(unittest.TestCase):
    def test_migrated_state_keeps_backup_and_export_excludes_raw_streams(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            run_dir = root / "run"
            run_dir.mkdir()
            old = {
                "state_schema_version": STATE_SCHEMA_VERSION - 1,
                "run_id": "run",
                "repo": str(root),
                "lead": "claude",
            }
            (run_dir / "state.json").write_text(json.dumps(old), encoding="utf-8")
            state = RunState.load(run_dir)
            self.assertEqual(STATE_SCHEMA_VERSION, state.state_schema_version)
            self.assertTrue(
                (run_dir / f"state.v{STATE_SCHEMA_VERSION - 1}.bak.json").exists()
            )
            attempt = run_dir / "attempts" / "a"
            attempt.mkdir(parents=True)
            (attempt / "stdout.jsonl").write_text("raw secret", encoding="utf-8")
            (attempt / "stderr.log").write_text("raw", encoding="utf-8")
            (attempt / "last-message.txt").write_text("raw", encoding="utf-8")
            (attempt / "events.jsonl").write_text("{}\n", encoding="utf-8")
            (attempt / "result.json").write_text("{}", encoding="utf-8")
            (attempt / "summary.json").write_text("{}", encoding="utf-8")
            (run_dir / "state.v1.bak.json").write_text(
                '{"token":"legacy-secret-value"}', encoding="utf-8"
            )
            outside = root / "outside.txt"
            outside.write_text("outside-secret", encoding="utf-8")
            linked = run_dir / "linked.txt"
            try:
                linked.symlink_to(outside)
            except OSError:
                linked = None
            output = export_run(run_dir, root / "run-export.zip")
            with zipfile.ZipFile(output) as archive:
                names = set(archive.namelist())
                manifest = json.loads(archive.read("export-manifest.json"))
            self.assertIn("state.json", names)
            self.assertIn("attempts/a/events.jsonl", names)
            self.assertNotIn("attempts/a/stdout.jsonl", names)
            self.assertNotIn("attempts/a/stderr.log", names)
            self.assertNotIn("state.v1.bak.json", names)
            if linked is not None:
                self.assertNotIn("linked.txt", names)
            self.assertFalse(manifest["raw_attempt_streams_included"])


if __name__ == "__main__":
    unittest.main()
