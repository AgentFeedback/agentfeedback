#!/usr/bin/env bash
# Publish a release from the pushed main: run the gates, build the release
# against a local tag, push the tag only when that build passed, upload the
# GitHub release and the image with GoReleaser (.goreleaser.yaml), then verify
# what was published against the local build. --check runs every precondition
# and a snapshot build of the whole matrix and the images, and publishes
# nothing.
# Run it through `just release` or `just release-check`; docs/releases.md is the
# procedure, recovery included.
set -euo pipefail

goreleaser_version=2.18.2
repo=AgentFeedback/agentfeedback
origin_re='github\.com[:/]AgentFeedback/agentfeedback(\.git)?/?$'
install_sh=skills/agentfeedback/scripts/install.sh
image=ghcr.io/agentfeedback/agentfeedback

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
# Smoke-test the per-platform images a snapshot build loaded (tags suffixed
# -amd64 and -arm64): on both platforms the binary prints the snapshot version,
# and the image started with no arguments (CMD serve) answers /ready. The
# smoke container and the snapshot images are removed on every exit path
# (remove_smoke runs from the EXIT traps too).
smoke="" smoke_version=""
remove_smoke() {
    local arch
    if [ -n "$smoke" ]; then docker rm -fv "$smoke" >/dev/null 2>&1 || true; smoke=""; fi
    if [ -n "$smoke_version" ]; then
        for arch in amd64 arm64; do
            docker image rm "$image:$smoke_version-$arch" "$image:$sha-$arch" >/dev/null 2>&1 || true
        done
        smoke_version=""
    fi
}
image_smoke() {
    local v=$1 arch ref out port
    for arch in amd64 arm64; do
        ref="$image:$v-$arch"
        out=$(docker run --rm --pull never --platform "linux/$arch" "$ref" version) ||
            die "$ref does not run on linux/$arch (on a machine of the other architecture this needs QEMU emulation, docs/releases.md)"
        [[ $out == "agentfeedback $v "* ]] || die "$ref prints '$out', expected version $v"
        smoke=agentfeedback-release-smoke-$$
        docker run -d --pull never --name "$smoke" --platform "linux/$arch" -e API_KEY=release-smoke-key -p 127.0.0.1::8080 "$ref" >/dev/null
        if ! port=$(docker port "$smoke" 8080/tcp | head -n1) || [ -z "$port" ] ||
            ! curl -fs -o /dev/null --retry 20 --retry-all-errors --retry-delay 1 --max-time 5 "http://127.0.0.1:${port##*:}/ready"; then
            docker logs "$smoke" >&2 || true
            die "$ref started with no arguments does not answer /ready"
        fi
        docker rm -fv "$smoke" >/dev/null
        smoke=""
    done
}

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

for tool in git gh goreleaser just python3 curl; do
    command -v "$tool" >/dev/null || die "$tool is not on PATH; docs/releases.md lists the tools"
done
docker buildx version >/dev/null || die "docker with buildx is required for the images and the playbook gate"
# GoReleaser pushes the image after the tag is public: check now that Docker
# holds a working login for ghcr.io (it reuses stored credentials and never
# prompts here). Write access to the package is not checkable without a push.
docker login ghcr.io </dev/null >/dev/null 2>&1 ||
    die "docker has no working login for ghcr.io; run docker login ghcr.io with a token that can write packages"
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
    trap remove_smoke EXIT
    smoke_version=$version-snapshot
    goreleaser release --snapshot --clean
    [ "$(listed dist/release/SHA256SUMS)" = "$(archives "$version-snapshot")" ] ||
        die "the snapshot's SHA256SUMS does not list exactly the six archives"
    verify_sums dist/release
    image_smoke "$version-snapshot"
    remove_smoke
    echo "release: ready to publish $tag at $sha"
    exit 0
fi

token=$(gh auth token) || die "gh has no token to give GoReleaser; run gh auth login"
[ -n "$token" ] || die "gh has no token to give GoReleaser; run gh auth login"

work=$(mktemp -d)
tag_created=false tag_pushed=false
finish() {
    local status=$?
    remove_smoke
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
# GoReleaser builds the images only while publishing, so a Dockerfile or image
# defect would surface after the tag is public: build and smoke-test them
# from a snapshot first. The release below rebuilds dist/release.
smoke_version=$version-snapshot
goreleaser release --snapshot --clean
image_smoke "$version-snapshot"
remove_smoke

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

# Verify the image as a puller sees it: GoReleaser pushed it before creating
# the release, tagged with the version and the commit, plus latest for a
# stable release. Every tag names the same index, which holds both platforms,
# and each platform's binary prints the version.
image_digest() { docker buildx imagetools inspect "$image:$1" --format '{{.Manifest.Digest}}'; }
digest=$(image_digest "$version") || die "image $image:$version is missing on GHCR"
[ "$(image_digest "$sha")" = "$digest" ] || die "image tag $sha does not match $version"
if [ "$prerelease" = false ] && [ "$(image_digest latest)" != "$digest" ]; then die "image tag latest does not match $version"; fi
if [ "$prerelease" = true ] && [ "$(image_digest latest 2>/dev/null || true)" = "$digest" ]; then die "pre-release image $version is tagged latest"; fi
platforms=$(docker buildx imagetools inspect "$image:$version" --format '{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}} {{end}}' |
    tr ' ' '\n' | grep -v '^unknown/' | grep . | sort | tr '\n' ' ')
[ "$platforms" = "linux/amd64 linux/arm64 " ] || die "image $image:$version holds '$platforms', expected linux/amd64 and linux/arm64"
for arch in amd64 arm64; do
    out=$(docker run --rm --pull always --platform "linux/$arch" "$image:$version" version) || die "image $image:$version does not run on linux/$arch"
    [[ $out == "agentfeedback $version "* ]] || die "image $image:$version on linux/$arch prints '$out'"
done

gh release view "$tag" --repo "$repo" --json url --jq .url
