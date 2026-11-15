# Setup and authentication

Applies to CLI 1.0.3, API v1/v2 (default v2). Select a real deployment URL;
OAuth scopes depend on the operation. Local discovery needs neither credentials
nor network. API version for execution and discovery resolves
`--api-version` → nonempty `PLANE_CLI_API_VERSION` → saved selected connection → `v2`. Saved v1 configurations remain v1; reject invalid explicit versions. No implicit destination or automatic version fallback. New connections resolve `--api-version` → nonempty `PLANE_CLI_API_VERSION` → `v2`, without inheriting the selected connection. Updates retain the stored version unless explicitly changed.

## Connect without leaking credentials

`$BASE_URL` below is the deployment root (including any path prefix), not an API
route. Have the operator inject `PLANE_CLI_API_KEY` securely for API-key mode;
never paste a token into commands, chat, fixtures or logs. Use HTTPS outside
controlled local fixtures. HTTP is supported but unencrypted.

```sh recipe
plane-cli connection add personal --base-url "$BASE_URL" --auth-mode api-key
plane-cli connection use personal
plane-cli connection show
plane-cli auth status --connection personal
```

These commands change only local configuration; `.data` reports local status,
not proof of API access. `connection show` does not read token values. Set
`PLANE_CLI_CONFIG_DIR` only to an isolated owner-only directory when isolation is
required. Existing named connections never silently fall back.

For persistent login, have the operator create a regular 0600 token file inside
an owner-only directory with safe ancestors, then run
`plane-cli auth login --connection personal --api-key-file /protected/token`.
Secret/callback files reject symlinks; `-` reads stdin. File backend storage is
owner-only **plaintext at rest**. Keyring is opt-in, has no fallback and depends
on native Keychain/Secret Service availability (not proved by mock tests).
`auth migrate --backend keyring --yes` changes storage; logout with `--yes`
removes local credentials, not remote grants. Do not authorize either implicitly.

## External injection and bearer/OAuth

For an operator-managed proxy, select `--auth-mode external` explicitly.
It sends neither API-key nor bearer header, even if token environment variables
exist. `auth status` reports `externally_managed`, not authenticated.
API-key mode uses `PLANE_CLI_API_KEY`; bearer/oauth-user/oauth-bot use
`PLANE_CLI_ACCESS_TOKEN`. Supplying both overrides is rejected. External access
tokens never refresh or write stored credentials.

OAuth requires a registered confidential app and its exact callback URI.
Discover `schema auth login` before choosing `--flow user|bot`,
`--client-id`, `--client-secret-file`, `--redirect-uri` and optional scopes.
Headless `--headless` returns `.data` containing pending-login details and a
sensitive consent URL, **not** authenticated success. Complete with
`auth complete --connection personal --login-id LOGIN_ID --callback-file -`.
Pending state is bound, single-use and expires after ten minutes. No device flow
or public-client PKCE is promised. Changing app defaults can revoke the old
local binding before consent; cancellation does not restore it.

An uncertain user refresh is never replayed. Saved-credential cleanup failure
can leave usable credentials: follow the reported explicit logout repair, not
another exchange. Never expose provider errors or consent URLs to shared logs.
Next: [work items](work-items.md); use `plane-cli auth login --help` for exact
local flags. API/OAuth redirects are refused; custom CAs use `--ca-file`.
