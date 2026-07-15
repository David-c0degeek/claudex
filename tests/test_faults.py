from __future__ import annotations

import json
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from claudex.events import EventJournal
from claudex.processes import AttemptPaths, stream_process
from claudex.state import (
    Phase,
    RunLockError,
    RunState,
    acquire_run_lock,
    release_run_lock,
)


class PersistenceFaultTests(unittest.TestCase):
    def test_failed_atomic_replace_preserves_previous_valid_state(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            state = RunState("run", ".", "claude", phase=Phase.INIT.value)
            state.save(run_dir)
            before = (run_dir / "state.json").read_bytes()
            state.phase = Phase.PLAN_DRAFT.value
            original_replace = Path.replace

            def fail_state_replace(path: Path, target: Path):
                if path.name == "state.json.tmp":
                    raise OSError("injected replace failure")
                return original_replace(path, target)

            with patch("pathlib.Path.replace", new=fail_state_replace):
                with self.assertRaisesRegex(OSError, "injected"):
                    state.save(run_dir)
            self.assertEqual(before, (run_dir / "state.json").read_bytes())
            loaded = RunState.load(run_dir)
            self.assertEqual(Phase.INIT.value, loaded.phase)

    def test_malformed_state_is_rejected_without_overwrite(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            path = run_dir / "state.json"
            path.write_text('{"run_id":', encoding="utf-8")
            before = path.read_bytes()
            with self.assertRaises(json.JSONDecodeError):
                RunState.load(run_dir)
            self.assertEqual(before, path.read_bytes())


class StreamFaultTests(unittest.TestCase):
    def test_stdout_reader_failure_does_not_deadlock_stderr_or_process_exit(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            attempt = AttemptPaths.create(root / "run", "reader-fault")
            original_open = Path.open

            def fail_stdout(path: Path, *args, **kwargs):
                if path == attempt.stdout:
                    raise OSError("injected reader failure")
                return original_open(path, *args, **kwargs)

            with patch("pathlib.Path.open", new=fail_stdout):
                result = stream_process(
                    [
                        sys.executable,
                        "-c",
                        "import sys; print('out'); print('err', file=sys.stderr)",
                    ],
                    cwd=root,
                    stdin_text="",
                    timeout=10,
                    attempt=attempt,
                    agent="fake",
                    phase="reader-fault",
                )
            # The child may report a platform-specific broken-pipe code. The
            # observable contract is prompt exit, drained stderr, and a
            # durable pipe-error event rather than a deadlock.
            self.assertIsInstance(result.returncode, int)
            self.assertLess(result.duration_s, 5)
            self.assertIn("err", attempt.stderr.read_text(encoding="utf-8"))
            warnings = [
                event
                for event in EventJournal(attempt.events).read()
                if event.provider_type == "pipe_error"
            ]
            self.assertTrue(warnings)

    def test_each_retry_gets_unique_attempt_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            attempts = []
            for _ in range(3):
                attempt = AttemptPaths.create(root / "run", "retry")
                attempts.append(attempt)
                stream_process(
                    [sys.executable, "-c", "print('attempt')"],
                    cwd=root,
                    stdin_text="",
                    timeout=10,
                    attempt=attempt,
                    agent="fake",
                    phase="retry",
                )
            self.assertEqual(3, len({attempt.attempt_id for attempt in attempts}))
            self.assertTrue(all(attempt.events.exists() for attempt in attempts))


class LockFaultTests(unittest.TestCase):
    def test_live_lock_contention_fails_promptly_and_releases_cleanly(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            acquire_run_lock(run_dir)
            started = time.monotonic()
            try:
                with self.assertRaises(RunLockError):
                    acquire_run_lock(run_dir)
            finally:
                release_run_lock(run_dir)
            self.assertLess(time.monotonic() - started, 1.0)
            acquire_run_lock(run_dir)
            release_run_lock(run_dir)


if __name__ == "__main__":
    unittest.main()
