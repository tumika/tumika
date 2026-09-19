# tumika desktop

The macOS tray app. It is a client of the tumika daemon exactly as the CLI is: it holds no
state of its own.

The app runs under the accessory activation policy, so it has no Dock icon and no menu bar of
its own — the tray icon is its whole presence. Clicking the icon opens a transparent, frameless
window positioned under the tray by `tauri-plugin-positioner`'s `TrayCenter`; clicking away
from it hides it again. The right button opens a menu with Quit.

## Toolchain

Rust is pinned per directory by `rust-toolchain.toml`; rustup reads it for any cargo command run
under `source/desktop/`. Node dependencies are managed with pnpm and `pnpm-lock.yaml` is
committed.

```sh
pnpm install
pnpm tauri dev              # run the app
pnpm tsc --noEmit           # typecheck
cd src-tauri && cargo clippy && cargo test
```
