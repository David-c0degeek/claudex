"""Git plumbing: worktree isolation, commit capture, diff extraction.

The lead implements in a dedicated worktree on a run-specific branch. The
pair reads the same worktree in read-only mode; the coordinator extracts
diffs itself so the pair reviews an exact, immutable artifact rather than
"review what the other agent just did".
"""

from __future__ import annotations

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
    # -uno: untracked files (build caches, __pycache__) are not review-relevant
    return bool(git(worktree, "status", "--porcelain", "-uno"))


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
