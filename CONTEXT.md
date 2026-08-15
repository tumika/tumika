# tumika — glossary

The vocabulary this repository uses. Terms only; no implementation detail. When a term here
appears in code, a commit message, an issue or an ADR, it means exactly this and nothing looser.

---

**workflow** — A deterministic, named unit of automation with a trigger (a schedule or an
event), a data-gathering phase that tumika performs itself, and one or more model calls over
the data it gathered. The first workflow is inbox triage. Workflows are *stage 2*; nothing in
this branch implements one.

**run** — A single execution of a workflow: one trigger firing, with its own inputs, outputs,
status and timing. A workflow is the definition; a run is the occurrence. *Stage 2.*

**provider** — An LLM backend tumika can send a rendered prompt to. A provider is identified by
a stable string ID (`claude-code`, `anthropic-api`), described by a **descriptor**, and
implemented by a **driver**. It is not a vendor: `claude-code` (the vendored CLI, driven as a
subprocess) and `anthropic-api` (the HTTP API) are two providers of the same vendor, because
they are installed, authenticated and invoked in entirely different ways.

**descriptor** — The static, public description of a provider: ID, display name, kind
(`cli` | `http`), its ordered list of **auth methods**, and whether tumika manages a binary for
it. It is what a client reads to decide how to render the provider, before any credential
exists.

**driver** — The implementation behind a provider. A driver declares what it can do by which
optional interfaces it implements, not by data.

**auth method** — How a credential for a provider is obtained: `api_key`, `manual_token` (the
user obtains the secret out-of-band and pastes it), or `interactive_cli` (a **login session** is
required). The first two are *non-interactive*; the third is *interactive*.

**credential** — A secret that authenticates tumika to a provider, together with its non-secret
metadata. The secret half never crosses the API boundary and never reaches a log; the metadata
half — hint, account email, status, issued/expires timestamps — is what clients see. A
credential is stored sealed.

**sealing** — Encrypting a credential's secret for storage, and decrypting it for use. The
ciphertext lives in the database; only the key lives outside it, in whichever custody backend
the platform provides. "Sealed" describes the stored form; "opened" describes the recovered
plaintext.

**login session** — A stateful, time-bounded, multi-step interaction that produces a credential
for a provider whose auth method is interactive: launch, surface an authorization URL, wait for
the user to approve in a browser, accept the code, capture the token, verify. Only interactive
auth methods create one. Submitting an API key does not — that is a single request with no
session.

**verification** — Establishing that a credential actually works, by using it against the
provider. Distinct from *validation*, which checks a secret's shape only and touches no
network. A credential that has been validated but not verified is `unverified`, not `active`.

**runner** — A supervised, long-lived process inside the daemon with a `Start`/`Stop` lifecycle,
started by the composition root and stopped on shutdown. Runners are the daemon's background
work: checking for updates, re-verifying credentials, and — in stage 2 — executing workflow
runs on schedule.

**connector** — An integration with an external system that tumika reads data from or writes
data to (a mailbox, a calendar). Connectors are the data plane; providers are the model plane.
tumika fetches and filters through a connector itself and hands the model a curated payload —
the model never gets direct access to the source. *Stage 2.*

**approval** — A point where a workflow pauses and waits for a human decision before an action
with external effect (sending a drafted reply). An approval is a first-class, persisted state,
not a prompt on a terminal. *Stage 2.*
