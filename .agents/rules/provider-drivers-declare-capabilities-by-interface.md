# A provider declares what it can do by implementing an interface, and its AuthMethods must match

Provider capabilities are discovered by **type assertion in the registry**, never by a boolean
field, a `switch` on the provider ID, or a method that returns `ErrNotSupported`.

| Interface | Required? | Meaning |
|---|---|---|
| `Provider` | **mandatory** | `Descriptor()` + `Preflight(ctx)` |
| `HealthChecker` | **mandatory** | `Verify(ctx, Credential) (CredentialMeta, error)` |
| `StaticAuthenticator` | optional | the caller already holds the secret: validate → materialize |
| `InteractiveAuthenticator` | optional | a multi-step login *session* is required |
| `Installer` | optional | tumika vendors and manages a binary for this provider |

A driver implements exactly the optional interfaces it truly supports. `AnthropicAPIDriver`
implements neither `Installer` nor `InteractiveAuthenticator` — that asymmetry is the entire
reason the design is capability-based, and it is why a second provider ships alongside the
first. An abstraction with one implementation is a guess.

**`Descriptor.AuthMethods` must agree with the interfaces actually implemented.** This is the
part that breaks silently:

- listing `interactive_cli` ⟺ implementing `InteractiveAuthenticator`;
- listing `api_key` or `manual_token` ⟹ implementing `StaticAuthenticator`, and
  `AcceptedMethods()` must return exactly the static subset of `AuthMethods`;
- `AuthMethods` is **ordered, preferred first** — a client renders the first one it supports.

Clients branch on the derived `RequiresInteractiveAuth()` to decide whether to render a plain
secret field or drive the login-session endpoints. A descriptor that overstates its methods
produces a UI that offers a flow the daemon will reject; one that understates them hides a
working path. Neither fails at compile time, which is why the conformance suite asserts the
correspondence for **every** registered driver.

Unsupported operations are refused at the service boundary with a stable sentinel, so the API
can return a documented code:

- `POST /v1/providers/{id}/login` on a non-interactive provider → `400 interactive_auth_unsupported`
- `POST /v1/providers/{id}/install` on a non-`Installer` provider → `400 install_unsupported`

## Applies to

| | |
|---|---|
| `source/internal/platform/provider/` | the interfaces and the registry's type assertions |
| `source/internal/platform/provider/claudecode/` | `Provider`, `HealthChecker`, `StaticAuthenticator` (`manual_token`), `Installer`, and (step 14) `InteractiveAuthenticator` |
| `source/internal/platform/provider/anthropicapi/` | `Provider`, `HealthChecker`, `StaticAuthenticator` (`api_key`) — and nothing else |
| `source/internal/service/` (`ProviderService`) | where the sentinel errors originate |
| the provider conformance test suite | run against **every** driver; asserts descriptor validity and the `AuthMethods` ⟺ interfaces correspondence |

## Example

**WRONG** — capability as data, plus a method that exists only to refuse:

```go
func (d *AnthropicAPIDriver) Descriptor() domain.Descriptor {
    return domain.Descriptor{
        ID:          "anthropic-api",
        AuthMethods: []domain.AuthMethod{domain.AuthAPIKey, domain.AuthInteractiveCLI}, // lie
        CanInstall:  false,                                                             // capability as a field
    }
}

func (d *AnthropicAPIDriver) Install(ctx context.Context, v string) (domain.InstallResult, error) {
    return domain.InstallResult{}, errors.New("not supported")   // satisfies Installer, supports nothing
}
```

The driver now satisfies `Installer`, so the registry's type assertion succeeds and
`POST …/install` returns a 500 from deep inside the driver instead of a documented 400. And
`RequiresInteractiveAuth()` is `true`, so every client renders a login flow that cannot exist.

**RIGHT** — the type is the declaration:

```go
func (d *AnthropicAPIDriver) Descriptor() domain.Descriptor {
    return domain.Descriptor{
        ID:          "anthropic-api",
        DisplayName: "Anthropic API",
        Kind:        "http",
        AuthMethods: []domain.AuthMethod{domain.AuthAPIKey},
        Managed:     false,
    }
}

func (d *AnthropicAPIDriver) AcceptedMethods() []domain.AuthMethod {
    return []domain.AuthMethod{domain.AuthAPIKey}
}
// No Install method at all. No InteractiveAuthenticator methods at all.
```

```go
// registry — capability discovery, once, in one place
func (r *Registry) Installer(id string) (provider.Installer, error) {
    p, err := r.Get(id)
    if err != nil { return nil, err }
    inst, ok := p.(provider.Installer)
    if !ok { return nil, domain.ErrInstallUnsupported }
    return inst, nil
}
```

## Why

Stage 2 adds an `Inferencer` interface implemented by both drivers. If capabilities were
fields or ID switches, that addition would touch the registry, every descriptor, and every
call site that guessed. As type assertions, it is a new accessor on the registry and two new
method sets — and any driver that does not implement it is refused at the boundary with a
sentinel, not a nil-pointer panic three layers down.
