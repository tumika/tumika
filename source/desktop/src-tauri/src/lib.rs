use tauri::image::Image;
use tauri::menu::{MenuBuilder, MenuItemBuilder};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{App, AppHandle, Manager, Runtime, WebviewWindow, WindowEvent};
use tauri_plugin_positioner::{Position, WindowExt};

/// Label of the window configured in `tauri.conf.json` as the tray popover.
const POPOVER: &str = "popover";

/// macOS renders a template image in the menu bar's own colour, so the asset is
/// black plus alpha and carries no colour of its own.
const TRAY_ICON: &[u8] = include_bytes!("../icons/tray-template.png");

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        // The positioner plugin has to be registered for `Position::TrayCenter`
        // to resolve, even though only Rust calls `move_window`.
        .plugin(tauri_plugin_positioner::init())
        .setup(|app| {
            set_accessory_activation_policy(app);
            build_tray(app)?;
            Ok(())
        })
        .on_window_event(|window, event| {
            // A popover dismisses itself when the user clicks elsewhere.
            if window.label() == POPOVER && matches!(event, WindowEvent::Focused(false)) {
                let _ = window.hide();
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

    TrayIconBuilder::with_id("tumika")
        .icon(Image::from_bytes(TRAY_ICON)?)
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

fn toggle_popover<R: Runtime>(app: &AppHandle<R>) {
    let Some(window) = app.get_webview_window(POPOVER) else {
        return;
    };

    if window.is_visible().unwrap_or(false) {
        let _ = window.hide();
        return;
    }
    show_popover(&window);
}

fn show_popover<R: Runtime>(window: &WebviewWindow<R>) {
    // Position before showing: moving a visible window makes it jump across the
    // screen from wherever it was last placed.
    let _ = window.move_window(Position::TrayCenter);
    let _ = window.show();
    let _ = window.set_focus();
}
