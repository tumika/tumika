# An HTTP handler decodes, calls exactly one service method, and encodes — nothing else

Every handler in `source/internal/api` is transport code. Its whole job is:

1. decode the request (path values, query, JSON body — subject to the body cap),
2. call **exactly one** service method,
3. map the returned value or error onto a status code and encode the response.

Anything that is not one of those three things belongs in `source/internal/service`.

Concretely, a handler must never:

- open a database handle, build SQL, or import `source/internal/repository` (depguard fails
  the build if it does);
- **branch on domain state** — `if cred.Status == "expired" { … }`, `if len(sessions) > 0 { … }`,
  "verify first, then store if it worked". Each of those is a business rule, and a rule split
  across two handlers is a rule that will diverge;
- orchestrate two service calls to make one outcome. If an operation needs "seal, then verify,
  then update status", that sequence is a transaction boundary, and transaction boundaries are
  the service layer's job. Two service calls from a handler means a partial failure has no
  owner.

The payoff is concrete and it is why the layering exists at all: **business logic is tested
against in-memory fakes of the repository interfaces, with no database and no HTTP.** Every
rule that leaks into a handler is a rule that now needs an `httptest` server to test, and one
that the CLI (which talks to the daemon over HTTP) can never reuse.

## Applies to

| | |
|---|---|
| `source/internal/api/**` | the rule binds here |
| `source/internal/service/**` | where the logic goes instead |
| `.golangci.yml` (`depguard`) | mechanical enforcement: `api` may not import `repository` |

`.golangci.yml` is the enforcement point for the import half of this rule. The
"no branching on domain state" half is not mechanically checkable — it is a review rule, and
it is the half that actually erodes.

## Example

**WRONG** — the handler owns the sequence, the failure modes, and a domain rule:

```go
func (h *Handler) putCredential(w http.ResponseWriter, r *http.Request) {
    var req putCredentialRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { … }

    p, err := h.providers.Get(r.PathValue("id"))       // registry lookup
    if err != nil { … }

    sa, ok := p.(provider.StaticAuthenticator)          // capability check
    if !ok {
        writeError(w, 400, "interactive_auth_unsupported", …)
        return
    }
    if err := sa.ValidateSecret(req.Method, req.Secret); err != nil { … }

    cred, _ := sa.Materialize(req.Method, req.Secret)
    meta, err := h.providerSvc.VerifyCredential(r.Context(), p.Descriptor().ID)  // call 1
    if err != nil { … }
    if meta.Status == "active" {                        // domain rule in a handler
        _ = h.providerSvc.StoreCredential(r.Context(), p.Descriptor().ID, cred) // call 2
    }
    writeJSON(w, 200, meta)
}
```

If `StoreCredential` fails after `VerifyCredential` succeeded, nobody owns the result. And the
"only store a credential that verified" rule now lives in `api`, where the CLI cannot reach it.

**RIGHT** — one call; the service owns validation, capability discovery, sealing, verification
and the transaction:

```go
func (h *Handler) putCredential(w http.ResponseWriter, r *http.Request) {
    var req putCredentialRequest
    if err := decodeJSON(r, &req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid_request", err)
        return
    }
    meta, err := h.providers.SubmitSecret(r.Context(), r.PathValue("id"), req.Method, req.Secret)
    if err != nil {
        writeServiceError(w, err)   // maps sentinel errors -> code + status, and nothing more
        return
    }
    writeJSON(w, http.StatusOK, meta)
}
```

`writeServiceError` is allowed to translate sentinel errors (`ErrInteractiveAuthUnsupported`
→ `400 interactive_auth_unsupported`) because that is encoding, not deciding: the service
already decided by returning that error.
