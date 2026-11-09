#!/usr/bin/env bash
set -euo pipefail
root=$(git rev-parse --show-toplevel)
# Git flake inputs exclude ignored files, including local credentials.
# Preserve absolute paths so linked worktrees can resolve their Git metadata.
args=(--rm -v "$root:$root:ro" -w "$root")
for d in "$(git rev-parse --path-format=absolute --git-dir)" \
         "$(git rev-parse --path-format=absolute --git-common-dir)"; do
    args+=(-v "$d:$d:ro")
done
docker run "${args[@]}" \
    nixos/nix@sha256:9305de534abed691d0f569502a4962a3a072ea8e888502c453ecfe35ae182867 \
    sh -ec 'git config --global --add safe.directory "$PWD"
        nix --extra-experimental-features "nix-command flakes" build . --no-link --no-update-lock-file
        expected=$(nix --extra-experimental-features "nix-command flakes" eval --raw .#packages.$(nix --extra-experimental-features nix-command eval --impure --raw --expr builtins.currentSystem).default.version)
        actual=$(nix --extra-experimental-features "nix-command flakes" run . --no-update-lock-file -- version)
        printf "%s\n" "$actual"
        case "$actual" in
            "agentfeedback $expected (commit "*) ;;
            *) echo "Nix binary version does not match package version: $expected" >&2; exit 1 ;;
        esac'
