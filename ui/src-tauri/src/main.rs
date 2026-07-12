// SyncDrive UI shell: a thin Tauri window over the React frontend. The Go
// daemon (`syncdrived`) is bundled as a sidecar binary and launched on
// startup; the frontend talks to it over the loopback JSON API.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use tauri_plugin_shell::process::CommandChild;
use tauri_plugin_shell::ShellExt;

struct DaemonHandle(std::sync::Mutex<Option<CommandChild>>);

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .manage(DaemonHandle(std::sync::Mutex::new(None)))
        .setup(|app| {
            // Launch the bundled daemon; ignore failure so the UI still opens
            // (it will show "daemon offline" and the user can start it manually).
            let handle = app.handle().clone();
            if let Ok(cmd) = handle.shell().sidecar("syncdrived") {
                if let Ok((_rx, child)) = cmd.args(["-port", "8737"]).spawn() {
                    let state: tauri::State<DaemonHandle> = handle.state();
                    *state.0.lock().unwrap() = Some(child);
                }
            }
            Ok(())
        })
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::Destroyed = event {
                // Stop the sidecar daemon when the last window closes.
                let state: tauri::State<DaemonHandle> = window.state();
                if let Some(child) = state.0.lock().unwrap().take() {
                    let _ = child.kill();
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("error while running SyncDrive");
}
