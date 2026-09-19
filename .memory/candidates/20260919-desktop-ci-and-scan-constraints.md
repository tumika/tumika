---
about:
  - .github/workflows/ci-build.yml
  - source/desktop/src-tauri/deny.toml
saw: 8a6624f
---

# Where the desktop app's CI and scan configuration has to live

- The desktop job is in `.github/workflows/ci-build.yml` because `ci-orchestration.yml` and
  `.github/dependabot.yml` are rendered by `gt repo sync` from `.gt-repo.yaml`; a job added to a
  rendered file is reverted by the next sync and drops out of `ci-gate` without anything failing.
  gt's template already gives the `npm` and `cargo` ecosystems the `build` commit-message prefix.
- `bulwark scan` runs cargo-deny over every Rust crate and eslint over every TypeScript project, so
  a new `source/desktop` tree fails it until `source/desktop/src-tauri/deny.toml` allows the
  licences its graph uses (cargo-deny's default allows none). MPL-2.0 arrives through tauri with no
  permissive alternative, so each MPL crate is a named `[[licenses.exceptions]]` entry rather than
  an `allow`, which makes a new MPL dependency fail the scan.
- bulwark's eslint step runs its own embedded config with `eslint-plugin-security`, not the
  project's `eslint.config.js`, so a clean `pnpm eslint .` is not sufficient; an indexed lookup
  such as `record[state]` trips `security/detect-object-injection` and is written as a `switch`.
- bulwark uses the ambient cargo, not the `rust-toolchain.toml` pin, so its Rust checks do not run
  under the toolchain CI uses.
