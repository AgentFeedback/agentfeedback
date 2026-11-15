# Install and explicitly check updates

Applies to skill 1.0.3, exact CLI 1.0.3, independent of Plane API
generation. Installation is explicit operator work; no automatic upgrades or
global skill path are assumed. Release metadata is generated, not a second
hand-maintained version counter. Snapshot CLI identity is `dev`, never the
unpublished release target. Snapshot metadata additionally declares `snapshot`.

```sh recipe
plane-cli version
plane-cli update check --help
plane-cli schema update check
```

Compare `.data.version` from `version` (optional leading `v`) with
`skill-metadata.json.cli_version` and `supported_cli_range`. Release metadata
contains `skill_version`, `cli_version`, `supported_cli_range`, `source_commit`;
released skills start exact-version only. Wrong version: obtain the matching
skill or use installed offline help/schema, never assume compatibility.

## Verified manual installation

Only consume artifacts from an explicitly approved release of `foae/plane-cli`.
This source does not claim any tag, release or Homebrew tap has been published.
Verify the SHA-256 manifest against each downloaded archive before extraction.
CLI archives are `plane-cli_X.Y.Z_OS_ARCH.tar.gz` for linux/darwin and
amd64/arm64. Inspect the target, LICENSE and executable; put `plane-cli` on PATH
without implicit sudo or a shell installer. Cross-builds are not native proof.
Tagged Go installation uses
`go install github.com/foae/plane-cli/cmd/plane-cli@vX.Y.Z` with the deliberately
selected tag. Homebrew needs the maintainer-selected published tap; do not guess
its name. The formula installs **only the CLI**, not this skill.

The matching `plane-cli-skill_X.Y.Z.tar.gz` contains a self-contained `plane-cli/`
directory, generated metadata and `LICENSE`. Extract it into a fresh
directory, inspect it, then explicitly copy that directory into the skill
location configured by the chosen host. Hosts supporting the standard
`SKILL.md` directory format can load it; e.g. Claude Code project skills use
`.claude/skills/plane-cli/`, and Codex project skills use
`.agents/skills/plane-cli/`. Verify the host's configured directory rather than
assuming a user/global path or support for a particular host version. Refuse to
overwrite an existing skill; retain or deliberately remove the old one only
with authorization. Relative references need no runtime remote docs.

## Explicit update check (not Plane releases)

Run `plane-cli update check` only when the user requests a network check.
`--include-prerelease` opts into preview selection. This queries public GitHub
release metadata over verified HTTPS with no Plane config/token/keyring access.
It does not download/install binaries, execute a shell, update a skill, cache
state, or start a background job. Ordinary startup/help/version never check.

The JSON success data includes `current_version`, nullable `latest_version`,
nullable `update_available`, `channel`, nullable `release_url`, and `status`:
`up_to_date`, `update_available`, `no_release`, or `unknown_current`.
An available update is exit 0. Development builds never guess their SemVer.
Network/rate-limit/malformed metadata are errors, not “up to date.” There is no
production endpoint override or arbitrary server-provided upgrade instruction.

If a new version is chosen explicitly: Homebrew users use their configured tap's
`brew upgrade plane-cli`; archive users verify and replace deliberately; Go users
select the exact tag. Install the matching versioned skill as a separate action.
After replacement, run `version` and recheck compatibility before API operations.
