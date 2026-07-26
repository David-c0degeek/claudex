#!/usr/bin/env bash
# Run a package's tests on real Linux from a Windows dev host, with no Go toolchain in WSL.
#
# The mechanical test gate's supervisor is Linux-only: it re-execs itself, becomes a child subreaper,
# owns a process group and reaps it. A cross-compile proves only that such code builds. This
# cross-compiles a standalone test binary and executes it under WSL, so the Linux paths are actually
# run rather than merely typechecked.
#
#   scripts/linux-test.sh ./internal/proctree/ -test.v -test.run TestSupervisor
set -euo pipefail

pkg="${1:?usage: linux-test.sh <package> [test flags...]}"
shift || true

distro="${CLAUDEX_WSL_DISTRO:-Ubuntu}"
# Forward slashes throughout: a Windows path with backslashes does not survive the shell hops below.
stage="${CLAUDEX_LINUX_TEST_STAGE:-C:/Users/$USERNAME/AppData/Local/Temp/claudex-linux-test}"
# C:/x -> /mnt/c/x
stage_wsl="$(printf '%s' "$stage" | sed -E 's#^([A-Za-z]):#/mnt/\l\1#')"

mkdir -p "$stage"
name="$(basename "$pkg").test"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$stage/$name" "$pkg"

# Deliberately free of command substitution: it does not survive the Windows -> WSL shell hops.
run_dir="/tmp/claudex-linux-test"
wsl.exe -d "$distro" -- bash -lc "set -eu; mkdir -p $run_dir && cp '$stage_wsl/$name' $run_dir/ && chmod +x $run_dir/$name && cd $run_dir && ./$name $*" 2>&1 | tr -d ''
