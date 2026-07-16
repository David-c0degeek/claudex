"""Git plumbing: worktree isolation, commit capture, diff extraction.

The lead implements in a dedicated worktree on a run-specific branch. The
pair reads the same worktree in read-only mode; the coordinator extracts
diffs itself so the pair reviews an exact, immutable artifact rather than
"review what the other agent just did".
"""

from __future__ import annotations

import hashlib
import json
import subprocess
from pathlib import Path


class GitError(RuntimeError):
    pass


def git(repo: Path, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", "-C", str(repo), *args],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    if check and proc.returncode != 0:
        raise GitError(
            f"git {' '.join(args)} failed (exit {proc.returncode}):\n{proc.stderr.strip()}"
        )
    return proc.stdout.strip()


def is_git_repo(repo: Path) -> bool:
    try:
        return git(repo, "rev-parse", "--is-inside-work-tree") == "true"
    except (GitError, FileNotFoundError):
        return False


def head_commit(repo: Path) -> str:
    return git(repo, "rev-parse", "HEAD")


def repo_name(repo: Path) -> str:
    return Path(git(repo, "rev-parse", "--show-toplevel")).name


def worktree_path_for(repo: Path, run_id: str) -> Path:
    """Sibling directory, so the worktree never nests inside the main
    checkout and neither agent can confuse the two."""
    top = Path(git(repo, "rev-parse", "--show-toplevel"))
    return top.parent / f"{top.name}.claudex.{run_id}"


def add_worktree(repo: Path, branch: str, path: Path, base: str) -> None:
    git(repo, "worktree", "add", "-b", branch, str(path), base)


def remove_worktree(repo: Path, path: Path, force: bool = False) -> None:
    args = ["worktree", "remove", str(path)]
    if force:
        args.append("--force")
    git(repo, *args)


def commits_between(worktree: Path, base: str) -> list[dict]:
    out = git(worktree, "log", "--format=%H%x09%s", f"{base}..HEAD")
    commits = []
    for line in out.splitlines():
        sha, _, subject = line.partition("\t")
        commits.append({"sha": sha, "message": subject})
    return commits


def diff_text(worktree: Path, base: str) -> str:
    return git(worktree, "diff", base, "HEAD")


def diff_stat(worktree: Path, base: str) -> str:
    return git(worktree, "diff", "--stat", base, "HEAD")


def has_uncommitted_changes(worktree: Path) -> bool:
    return bool(status_porcelain(worktree))


def tracked_paths(repo: Path, *pathspecs: str) -> list[str]:
    """Return tracked files under the supplied repository-relative pathspecs."""
    output = git(repo, "ls-files", "--", *pathspecs)
    return [line for line in output.splitlines() if line]


def path_is_ignored(repo: Path, path: str) -> bool:
    """Check ignore coverage even when the probe path does not exist."""
    return bool(git(repo, "check-ignore", "--no-index", "--", path, check=False))


def status_porcelain(worktree: Path) -> str:
    """Return all tracked and untracked changes; ignored files stay ignored."""
    return git(
        worktree,
        "status",
        "--porcelain=v1",
        "--untracked-files=all",
    )


def tree_id(worktree: Path, rev: str = "HEAD") -> str:
    return git(worktree, "rev-parse", f"{rev}^{{tree}}")


def worktree_identity(worktree: Path) -> dict[str, object]:
    """Hash the exact visible worktree, including untracked file bytes."""
    head = head_commit(worktree)
    status = status_porcelain(worktree)
    patch = git(worktree, "diff", "--binary", "HEAD")
    staged = git(worktree, "diff", "--binary", "--cached", "HEAD")
    untracked = git(
        worktree, "ls-files", "--others", "--exclude-standard", "-z"
    ).split("\0")
    untracked_files: list[dict[str, object]] = []
    for relative in sorted(item for item in untracked if item):
        path = worktree / relative
        if not path.is_file():
            continue
        content = path.read_bytes()
        untracked_files.append(
            {
                "path": relative.replace("\\", "/"),
                "bytes": len(content),
                "sha256": hashlib.sha256(content).hexdigest(),
            }
        )
    payload = {
        "head": head,
        "head_tree": tree_id(worktree),
        "status": status,
        "patch_sha256": hashlib.sha256(patch.encode("utf-8")).hexdigest(),
        "staged_patch_sha256": hashlib.sha256(staged.encode("utf-8")).hexdigest(),
        "untracked": untracked_files,
    }
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"))
    payload["content_sha256"] = hashlib.sha256(encoded.encode("utf-8")).hexdigest()
    return payload


def ensure_gitignore_entry(repo: Path, entry: str) -> bool:
    """Add entry to .gitignore if missing. Returns True if it was added."""
    gi = repo / ".gitignore"
    if gi.exists():
        lines = gi.read_text(encoding="utf-8").splitlines()
        if entry in (line.strip() for line in lines):
            return False
        with gi.open("a", encoding="utf-8") as f:
            if lines and lines[-1].strip():
                f.write("\n")
            f.write(f"{entry}\n")
    else:
        gi.write_text(f"{entry}\n", encoding="utf-8")
    return True
