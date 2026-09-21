pub mod bomsig;
mod health;
mod keychain;
pub mod pairing;
mod status;
pub mod update;

use std::sync::Mutex;
use std::time::{Duration, Instant};

use tauri::image::Image;
use tauri::menu::{MenuBuilder, MenuItemBuilder};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{App, AppHandle, Emitter, Manager, Runtime, State, WebviewWindow, WindowEvent};
use tauri_plugin_positioner::{Position, WindowExt};

use health::DEFAULT_BASE_URL;
use keychain::SecurityCli;
use status::{icon_for, DaemonStatus, IconKind, Monitor};
use update::SharedStatus;

/// Label of the window configured in `tauri.conf.json` as the tray popover.
const POPOVER: &str = "popover";

/// Id of the tray icon, needed to reach it again when the state changes.
const TRAY: &str = "tumika";

/// Event carrying a [`DaemonStatus`] to the popover after every poll.
const STATUS_EVENT: &str = "daemon-status";

/// How often the daemon is asked how it is. Frequent enough that the tray is
/// worth glancing at, rare enough to cost nothing on a laptop.
const POLL_INTERVAL: Duration = Duration::from_secs(10);

/// How long after a focus loss hid the popover a tray click still counts as the
/// click that caused it. Long enough to cover the gap between the two events,
/// short enough that a deliberate second click reopens the popover.
const DISMISS_GRACE: Duration = Duration::from_millis(200);

/// The most recent poll, or `None` before the first one has finished — which is
/// what the popover renders as "Checking…".
#[derive(Default)]
struct CurrentStatus(Mutex<Option<DaemonStatus>>);

/// When a focus loss last hid the popover, cleared as soon as a tray click has
/// been judged against it.
#[derive(Default)]
struct LastDismissal(Mutex<Option<Instant>>);

/// Answers the popover's first render, which happens before any event arrives.
#[tauri::command]
fn daemon_status(current: State<'_, CurrentStatus>) -> Option<DaemonStatus> {
    current.0.lock().expect("status lock").clone()
}

/// Lets the popover's Quit button end the app; the tray menu's item does the same.
#[tauri::command]
fn quit(app: AppHandle) {
    app.exit(0);
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        // The positioner plugin has to be registered for `Position::TrayCenter`
        // to resolve, even though only Rust calls `move_window`.
        .plugin(tauri_plugin_positioner::init())
        // The updater's public key comes from `plugins.updater.pubkey`; the
        // endpoint carrying each decision is built at runtime, so none is
        // configured. Only Rust drives it, so no capability grants the webview
        // its commands.
        .plugin(tauri_plugin_updater::Builder::new().build())
        .manage(CurrentStatus::default())
        .manage(SharedStatus::default())
        .manage(LastDismissal::default())
        .invoke_handler(tauri::generate_handler![daemon_status, quit])
        .setup(|app| {
            set_accessory_activation_policy(app);
            build_tray(app)?;
            start_polling(app.handle().clone())?;
            start_pairing(app.handle().clone())?;
            Ok(())
        })
        .on_window_event(|window, event| {
            // A popover dismisses itself when the user clicks elsewhere.
            if window.label() == POPOVER && matches!(event, WindowEvent::Focused(false)) {
                let _ = window.hide();
                // The click that took the focus may be a click on the tray
                // icon, whose own event arrives afterwards; the instant is what
                // lets `toggle_popover` recognise it.
                *window
                    .app_handle()
                    .state::<LastDismissal>()
                    .0
                    .lock()
                    .expect("dismissal lock") = Some(Instant::now());
            }
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}

/// Accessory keeps the app out of the Dock and out of the app switcher, which is
/// what makes the tray icon its only presence.
fn set_accessory_activation_policy(app: &mut App) {
    #[cfg(target_os = "macos")]
    app.set_activation_policy(tauri::ActivationPolicy::Accessory);
    #[cfg(not(target_os = "macos"))]
    let _ = app;
}

fn build_tray(app: &mut App) -> tauri::Result<()> {
    let quit = MenuItemBuilder::with_id("quit", "Quit Tumika").build(app)?;
    let menu = MenuBuilder::new(app).item(&quit).build()?;

    TrayIconBuilder::with_id(TRAY)
        // The first poll is issued as soon as the app is up and replaces this,
        // so the all-clear image is only the tray's resting appearance.
        .icon(Image::from_bytes(icon_bytes(IconKind::AllClear))?)
        .icon_as_template(true)
        // The left button opens the popover, so the menu belongs to the right one.
        .show_menu_on_left_click(false)
        .menu(&menu)
        .on_menu_event(|app, event| {
            if event.id() == "quit" {
                app.exit(0);
            }
        })
        .on_tray_icon_event(|tray, event| {
            // The plugin tracks the tray's screen rectangle from these events; it
            // has no other source for it, so every event is forwarded.
            tauri_plugin_positioner::on_tray_event(tray.app_handle(), &event);

            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                toggle_popover(tray.app_handle());
            }
        })
        .build(app)?;

    Ok(())
}

/// Bytes of the menu bar image for each state.
///
/// macOS renders a template image in the menu bar's own colour, so every asset
/// is black plus alpha and carries no colour of its own.
const fn icon_bytes(kind: IconKind) -> &'static [u8] {
    match kind {
        IconKind::AllClear => include_bytes!("../icons/tray-all-clear.png"),
        IconKind::NeedsYou => include_bytes!("../icons/tray-needs-you.png"),
        IconKind::Stopped => include_bytes!("../icons/tray-stopped.png"),
    }
}

/// Starts the poll loop that is the app's only source of daemon state.
fn start_polling(app: AppHandle) -> Result<(), Box<dyn std::error::Error>> {
    let monitor = Monitor::new(Box::new(SecurityCli), DEFAULT_BASE_URL)?;

    tauri::async_runtime::spawn(async move {
        loop {
            let status = monitor.poll().await;
            publish(&app, status);
            tokio::time::sleep(POLL_INTERVAL).await;
        }
    });

    Ok(())
}

/// Starts the loop that keeps this app on the desktop component version its
/// daemon's release names.
fn start_pairing(app: AppHandle) -> Result<(), Box<dyn std::error::Error>> {
    let status = app.state::<SharedStatus>().inner().clone();
    update::start(app.clone(), status, DEFAULT_BASE_URL)?;
    Ok(())
}

/// Records a poll, points the tray at the icon it calls for, and tells the
/// popover.
fn publish(app: &AppHandle, status: DaemonStatus) {
    if let Some(tray) = app.tray_by_id(TRAY) {
        if let Ok(icon) = Image::from_bytes(icon_bytes(icon_for(status.state))) {
            let _ = tray.set_icon(Some(icon));
            let _ = tray.set_icon_as_template(true);
        }
    }

    *app.state::<CurrentStatus>().0.lock().expect("status lock") = Some(status.clone());
    let _ = app.emit(STATUS_EVENT, status);
}

fn toggle_popover<R: Runtime>(app: &AppHandle<R>) {
    let Some(window) = app.get_webview_window(POPOVER) else {
        return;
    };

    if window.is_visible().unwrap_or(false) {
        let _ = window.hide();
        return;
    }

    // Clicking the tray while the popover is open takes the focus away first, so
    // the focus-loss handler has already hidden the window by the time this
    // click's mouse-up arrives and the popover looks closed. Whether the hide or
    // the click lands first is the window manager's business; the grace window
    // is what absorbs that ordering.
    let dismissal = app.state::<LastDismissal>();
    let mut dismissed_at = dismissal.0.lock().expect("dismissal lock");
    let open = should_open(*dismissed_at, Instant::now());
    // Judged once: a click that is absorbed is the end of the gesture, and the
    // next one opens the popover.
    *dismissed_at = None;

    if open {
        show_popover(&window);
    }
}

/// Whether a tray click landing on a hidden popover opens it.
///
/// A click within [`DISMISS_GRACE`] of a focus loss that hid the popover is the
/// same gesture as that dismissal, so it closes the popover rather than
/// reopening it.
fn should_open(dismissed_at: Option<Instant>, now: Instant) -> bool {
    dismissed_at.is_none_or(|at| now.saturating_duration_since(at) >= DISMISS_GRACE)
}

fn show_popover<R: Runtime>(window: &WebviewWindow<R>) {
    // Position before showing: moving a visible window makes it jump across the
    // screen from wherever it was last placed.
    let _ = window.move_window(Position::TrayCenter);
    let _ = window.show();
    let _ = window.set_focus();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_click_just_after_a_focus_loss_leaves_the_popover_closed() {
        let dismissed_at = Instant::now();
        let click = dismissed_at + DISMISS_GRACE / 2;

        assert!(!should_open(Some(dismissed_at), click));
    }

    #[test]
    fn a_click_long_after_a_focus_loss_opens_the_popover() {
        let dismissed_at = Instant::now();
        let click = dismissed_at + DISMISS_GRACE * 2;

        assert!(should_open(Some(dismissed_at), click));
    }

    #[test]
    fn a_click_on_a_popover_no_focus_loss_ever_hid_opens_it() {
        assert!(should_open(None, Instant::now()));
    }
}
