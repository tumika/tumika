# Every spawned `claude` process is credential-isolated, and its auth method is asserted afterwards

There is exactly one place in tumika that builds a `*exec.Cmd` for the vendored `claude`
binary, and every caller — install, preflight, verify, PTY login, and stage 2's inference —
goes through it. It applies all five of the following. They are **one policy**; any one
missing puts the user on API billing without telling them.

**1. Scrub the credential-precedence environment.** Start from a minimal environment, not
`os.Environ()`. Explicitly ensure these are absent: `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_PROFILE`,
`CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `CLAUDE_CODE_USE_FOUNDRY`, and the cloud
provider variables those imply.

**2. Always pass `--setting-sources ''`.** This is the one that is easy to omit and impossible
to notice. Credential precedence is:

```
cloud vars → ANTHROPIC_AUTH_TOKEN → ANTHROPIC_API_KEY → apiKeyHelper → CLAUDE_CODE_OAUTH_TOKEN
```

`apiKeyHelper` is a **settings-file key**, not an environment variable, and it outranks the
token tumika injects. A perfectly scrubbed environment does not touch it. Only refusing to load
settings files does.

**3. Set an isolated `CLAUDE_CONFIG_DIR`** under tumika's own data root, so the daemon never
reads or mutates the operator's personal Claude Code configuration.

**4. Set `DISABLE_AUTOUPDATER=1`.** Claude Code updates itself by default. tumika pins an exact
version (currently **2.1.232**) because the login flow parses that version's TUI output; a
silent self-update is a silent break. The pin is also why the binary is executed by **absolute
path** — no launcher symlink that could be repointed.

**5. Never pass `--bare`.** It does not read `CLAUDE_CODE_OAUTH_TOKEN`, so it silently falls
through the precedence chain to whatever is next.

And then, because none of the above is observable from the outside, **verification asserts the
outcome**:

```
claude auth status --json   →   authMethod MUST equal "oauth_token"
```

That single assertion is what converts a silent misroute to API billing into a loud error.

## Verification keys on `is_error`, never `subtype`

Two probed facts make the obvious check wrong:

- `claude auth status --json` reports `loggedIn: true` **for a completely bogus token**. It is
  useless as a validity check. Read `authMethod` / `apiKeySource` from it and nothing else.
- `claude -p` with a bad token returns `subtype: "success"` **alongside** `is_error: true,
  api_error_status: 401`.

So `Verify` is two stages, and the second one keys on `is_error` and `api_error_status`:

```
claude -p "Reply with the single word: ok" --output-format json --max-turns 1 --setting-sources ''
```

## Applies to

| | |
|---|---|
| `source/internal/platform/provider/claudecode/` | the single `*exec.Cmd` constructor — the only place a `claude` process is built |
| `…/claudecode` `Verify` | the two-stage check: `authMethod == "oauth_token"`, then `is_error` |
| `…/claudecode` PTY login (step 14) | spawns through the same constructor; a PTY does not exempt it |
| the precedence regression test | runs `Verify` with `ANTHROPIC_API_KEY` set in the daemon's own environment and asserts `authMethod` is still `oauth_token` |

## Example

**WRONG**:

```go
cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--output-format", "json")
cmd.Env = filterOut(os.Environ(), "ANTHROPIC_API_KEY")   // apiKeyHelper is untouched
out, _ := cmd.Output()

var res claudeResult
_ = json.Unmarshal(out, &res)
if res.Subtype == "success" {        // true even for a 401
    return meta, nil
}
```

Resolved by PATH (so a user-installed `claude` may run instead of the pinned one), settings
files still load, the auto-updater is live, and the result check passes on an authentication
failure.

**RIGHT**:

```go
cmd := d.command(ctx, "-p", prompt, "--output-format", "json", "--max-turns", "1")
out, err := cmd.Output()
if err != nil { return domain.CredentialMeta{}, err }

var res claudeResult
if err := json.Unmarshal(out, &res); err != nil { return domain.CredentialMeta{}, err }
if res.IsError {
    return domain.CredentialMeta{}, fmt.Errorf("%w: api_error_status=%d",
        domain.ErrCredentialInvalid, res.APIErrorStatus)
}
```

```go
// the single constructor: absolute path, minimal env, mandatory flags
func (d *Driver) command(ctx context.Context, args ...string) *exec.Cmd {
    args = append([]string{"--setting-sources", ""}, args...)
    cmd := exec.CommandContext(ctx, d.binPath, args...)   // absolute, pinned version
    cmd.Env = []string{
        "PATH=" + minimalPath,
        "HOME=" + d.home,
        "CLAUDE_CONFIG_DIR=" + d.configDir,
        "DISABLE_AUTOUPDATER=1",
        "CLAUDE_CODE_OAUTH_TOKEN=" + d.token,
    }
    return cmd
}
```

## Why

The failure mode has no symptom. Everything works: the daemon starts, workflows run, replies
come back correctly. The only difference is that the operator is being billed API rates for
every request instead of using the subscription they installed tumika to use — and they find
out from an invoice weeks later. The whole reason tumika drives the CLI rather than the API is
cost, so silently routing to the API defeats the product's premise.

`apiKeyHelper` makes it likely rather than theoretical: it is a plausible thing to find in a
developer's `settings.json`, it is invisible to `env`, and it outranks the token tumika went to
considerable trouble to obtain.
