#!/usr/bin/env bash
# Run a package's tests on real Linux from a Windows dev host, with no Go toolchain in WSL.
#
# The mechanical test gate's supervisor is Linux-only: it re-execs itself, becomes a child subreaper,
# owns a process group and reaps it. A cross-compile proves only that such code builds. This
# cross-compiles a standalone test binary and executes it under WSL, so the Linux paths are actually
# run rather than merely typechecked.
#
#   scripts/linux-test.sh ./internal/proctree/ -test.v -test.run 'TestA|TestB'
#
# Test flags are passed through as argv, NOT spliced into a shell command. An earlier version
# interpolated "$*" into `bash -lc`, so a Go regex containing '|' became a pipeline and MSYS rewrote
# flags that looked like paths: the runner silently ran something other than what was asked for, which
# is worse than not running at all.
set -euo pipefail

pkg="${1:?usage: linux-test.sh <package> [test flags...]}"
shift || true

distro="${CLAUDEX_WSL_DISTRO:-Ubuntu}"
# Forward slashes throughout: a Windows path with backslashes does not survive the shell hops below.
stage="${CLAUDEX_LINUX_TEST_STAGE:-C:/Users/$USERNAME/AppData/Local/Temp/claudex-linux-test}"
# C:/x -> /mnt/c/x
stage_wsl="$(printf '%s' "$stage" | sed -E 's#^([A-Za-z]):#/mnt/\l\1#')"

# Invocation-unique, so concurrent runs cannot overwrite or execute each other's binary.
id="$$-${RANDOM}"
name="$(basename "$pkg").${id}.test"
run_dir="/tmp/claudex-linux-test/${id}"

mkdir -p "$stage"
cleanup() {
  rm -f "$stage/$name" 2>/dev/null || true
  wsl.exe -d "$distro" -- bash -lc "rm -rf '$run_dir'" >/dev/null 2>&1 || true
}
trap cleanup EXIT

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$stage/$name" "$pkg"

# Staging only; no user-supplied text reaches this shell.
wsl.exe -d "$distro" -- bash -lc "set -eu; mkdir -p '$run_dir' && cp '$stage_wsl/$name' '$run_dir/' && chmod +x '$run_dir/$name'"

# wsl.exe JOINS argv into a single command string and runs it through a shell on the Linux side, so
# argv cannot be handed across untouched. The only correct move is to quote for that shell explicitly:
# each argument is wrapped in single quotes with embedded quotes escaped, so a Go regex containing '|'
# arrives as one word instead of becoming a pipeline.
shquote() {
  # Parameter expansion, not sed: in a sed replacement \' collapses to a bare quote, which turns an
  # embedded apostrophe into ''' and breaks the quoting it was supposed to preserve.
  local esc="'\\''"
  printf "'%s'" "${1//\'/$esc}"
}

quoted=""
for a in "$@"; do
  quoted="$quoted $(shquote "$a")"
done

MSYS2_ARG_CONV_EXCL='*' wsl.exe -d "$distro" -- bash -lc "cd '$run_dir' && exec './$name'$quoted"
