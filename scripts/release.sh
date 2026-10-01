#!/usr/bin/env bash
# Publish a release from the pushed main: run the gates, build the release
# against a local tag, push the tag only when that build passed, upload the
# GitHub release with GoReleaser (.goreleaser.yaml), verify what was published
# against the local build, then push the image. --check runs every
# precondition and a snapshot build of the whole matrix, and publishes nothing.
# Run it through `just release` or `just release-check`; docs/releases.md is the
# procedure, recovery included.
set -euo pipefail

goreleaser_version=2.18.2
repo=AgentFeedback/agentfeedback
origin_re='github\.com[:/]AgentFeedback/agentfeedback(\.git)?/?$'
install_sh=skills/agentfeedback/scripts/install.sh

die() { echo "release: $*" >&2; exit 1; }
verify_sums() { (cd "$1" && if command -v sha256sum >/dev/null; then sha256sum -c SHA256SUMS; else shasum -a 256 -c SHA256SUMS; fi); }
# The six archive names for a version, sorted.
archives() {
    for os in darwin linux windows; do
        for arch in amd64 arm64; do
            ext=tar.gz
            if [ "$os" = windows ]; then ext=zip; fi
            echo "agentfeedback_$1_${os}_${arch}.$ext"
        done
    done | sort
}
listed() { awk '{ print $2 }' "$1" | sort; }

check=false
if [ "${1:-}" = --check ]; then check=true; shift; fi
[ $# -eq 3 ] || die "usage: release.sh [--check] <vX.Y.Z|vX.Y.Z-rc.N> <title> <notes.md>"
tag=$1 title=$2 notes=$3

[[ $tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
    die "expected a tag vX.Y.Z or vX.Y.Z-rc.N, got '$tag'"
version=${tag#v}
prerelease=false
if [[ $tag == *-rc.* ]]; then prerelease=true; fi
[ -n "${title//[[:space:]]/}" ] || die "a release title is required"
if [ ! -f "$notes" ] || ! grep -q '[^[:space:]]' "$notes"; then die "the notes file '$notes' is missing or empty"; fi
notes=$(cd "$(dirname "$notes")" && pwd)/$(basename "$notes")

cd "$(git rev-parse --show-toplevel)"

for tool in git gh goreleaser just python3; do
    command -v "$tool" >/dev/null || die "$tool is not on PATH; docs/releases.md lists the tools"
done
if [ "$check" = false ]; then
    docker buildx version >/dev/null || die "docker with buildx is required for the playbook gate and the image push"
fi
have=$(goreleaser --version | awk '$1 == "GitVersion:" { print $2 }')
[ "$have" = "$goreleaser_version" ] ||
    die "GoReleaser $goreleaser_version is pinned, found '${have:-unknown}'; install that version"
[ -f "$install_sh" ] || die "$install_sh is missing; every release publishes it as an asset"

# Tags go to origin and the release to $repo (and .goreleaser.yaml's
# release.github): they must be the same repository.
origin_url=$(git remote get-url origin)
shopt -s nocasematch
[[ $origin_url =~ $origin_re ]] || die "origin is $origin_url, not github.com/$repo"
shopt -u nocasematch
[ -z "$(git status --porcelain)" ] || die "commit all changes before releasing"
[ "$(git branch --show-current)" = main ] || die "release from main"
sha=$(git rev-parse HEAD)
remote_main=$(git ls-remote origin refs/heads/main | cut -f1)
[ "$remote_main" = "$sha" ] || die "push this exact commit to origin/main before releasing"
# An unreachable origin fails here rather than reading as "no such tag".
remote_tag=$(git ls-remote origin "refs/tags/$tag")
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null || [ -n "$remote_tag" ]; then
    die "tag $tag already exists; never replace a published version"
fi
# The tag is pushed over git, the release over the gh token: check the token
# can write before anything becomes public.
[ "$(gh api "repos/$repo" --jq .permissions.push)" = true ] ||
    die "gh is authenticated as $(gh api user --jq .login), which cannot push to $repo"

# The pinned toolchain is go.mod's toolchain line, whatever go is on PATH. The
# tag is named explicitly: GoReleaser would otherwise pick among the tags on
# HEAD, and a stable tag usually shares its commit with the last rc.
GOTOOLCHAIN=$(awk '$1 == "toolchain" { print $2 }' go.mod)
[ -n "$GOTOOLCHAIN" ] || die "go.mod has no toolchain line"
export GOTOOLCHAIN GORELEASER_CURRENT_TAG=$tag RELEASE_TITLE=$title
goreleaser check

# There is no hosted CI: the gates run here, on the exact commit that is tagged.
just ci
[ -z "$(git status --porcelain)" ] || die "just ci left changes in the tree"

if [ "$check" = true ]; then
    goreleaser release --snapshot --clean
    [ "$(listed dist/release/SHA256SUMS)" = "$(archives "$version-snapshot")" ] ||
        die "the snapshot's SHA256SUMS does not list exactly the six archives"
    verify_sums dist/release
    echo "release: ready to publish $tag at $sha"
    exit 0
fi

token=$(gh auth token) || die "gh has no token to give GoReleaser; run gh auth login"
[ -n "$token" ] || die "gh has no token to give GoReleaser; run gh auth login"

work=$(mktemp -d)
tag_created=false tag_pushed=false
finish() {
    local status=$?
    rm -rf "$work"
    if [ "$tag_created" = true ] && [ "$tag_pushed" = false ]; then git tag -d "$tag" >/dev/null 2>&1 || true; fi
    if [ "$status" -ne 0 ] && [ "$tag_pushed" = true ]; then
        echo "release: $tag is public but the release did not finish; follow docs/releases.md#recovery and never move the tag" >&2
    fi
}
trap finish EXIT

# Build the release for real against a local tag first: a defect in the build
# or the archives stops here, and the local tag is removed on the way out.
git tag -a "$tag" "$sha" -m "$tag: $title"
tag_created=true
goreleaser release --clean --skip=publish
[ "$(listed dist/release/SHA256SUMS)" = "$(archives "$version")" ] ||
    die "the local build's SHA256SUMS does not list exactly the six archives"
verify_sums dist/release
cp dist/release/SHA256SUMS "$work/SHA256SUMS.local"
# Both install playbooks, run in a clean container against the local build:
# a playbook that fails on these assets stops the release before the tag is
# public.
just playbooks "$tag" dist/release

git push --force-with-lease="refs/tags/$tag:" origin "refs/tags/$tag"
tag_pushed=true
GITHUB_TOKEN=$token goreleaser release --clean --release-notes "$notes"

# Verify the release as a downloader sees it. GoReleaser rebuilt it; the build
# is reproducible, so it must match the local build byte for byte.
state=$(gh release view "$tag" --repo "$repo" --json tagName,isDraft,isPrerelease --jq '"\(.tagName) \(.isDraft) \(.isPrerelease)"')
[ "$state" = "$tag false $prerelease" ] ||
    die "release verification failed (tag draft prerelease: $state); inspect GitHub without moving the tag"
latest=$(gh api "repos/$repo/releases/latest" --jq .tag_name 2>/dev/null) || latest="(none)"
if [ "$prerelease" = true ] && [ "$latest" = "$tag" ]; then die "pre-release $tag is marked latest; unmark it on GitHub"; fi
if [ "$prerelease" = false ] && [ "$latest" != "$tag" ]; then die "$tag is not marked latest (latest is $latest)"; fi
actual=$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[].name' | sort)
[ "$actual" = "$( (archives "$version"; echo SHA256SUMS; echo install.sh) | sort)" ] ||
    die "release assets differ from the expected set; got: $(echo "$actual" | tr '\n' ' ')"
gh release download "$tag" --repo "$repo" --dir "$work/download" --pattern 'agentfeedback_*' --pattern SHA256SUMS --pattern install.sh
cmp -s "$work/SHA256SUMS.local" "$work/download/SHA256SUMS" ||
    die "the published SHA256SUMS differs from the local build's"
cmp -s "$install_sh" "$work/download/install.sh" || die "the published install.sh differs from $install_sh"
verify_sums "$work/download"

# The image is published only here: the version and the commit, plus latest
# for a stable release.
image_tags=("$version" "$sha")
if [ "$prerelease" = false ]; then image_tags+=(latest); fi
just image-push "${image_tags[@]}"

gh release view "$tag" --repo "$repo" --json url --jq .url
