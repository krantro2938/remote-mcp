# remote-mcp

Lets Claude see and control a Windows computer (for example, a lab PC running Vivado) through MCP. You can use it to take screenshots for reports, click through GUIs and run commands.

```
[Windows PC]                         [VPS]                                  [Claude]
 agent.exe ──wss, outbound only──▶  relay: /agent  (agent hub)              claude.ai custom connector
 screenshots / input / commands     relay: /mcp    (MCP + OAuth)  ◀──HTTPS── or Claude Code
                                    Caddy: HTTPS
```

* **agent.exe** is a single binary with no install and no admin rights. It connects *out* to the relay, so it works behind NAT and on university networks.
* **relay** is one Go binary on your VPS. It keeps the agent connections, serves the MCP endpoint, stores full-resolution captures, and handles OAuth for claude.ai.

## 1. Set up the VPS (Debian/Ubuntu)

```bash
git clone https://github.com/krantro2938/remote-mcp && cd remote-mcp
sudo ./deploy/install.sh                     # domain = <your-ip>.sslip.io, no DNS needed
# or: sudo ./deploy/install.sh relay.example.com   (A record → VPS IP)
```

The script installs Go and Caddy, builds everything, generates secrets in `/etc/remote-mcp/relay.env`, starts the `remote-mcp` service, gets an HTTPS certificate, and prints:

* the MCP URL `https://<domain>/mcp`
* the **admin password**, used for the OAuth "Allow" page, the gallery and downloads
* agent tokens for the machines `laptop` and `labpc` (set `MACHINES="a b c"` to change the names)

To update, run `git pull && sudo ./deploy/install.sh` again. Your secrets are kept.

## 2. Start the agent on Windows

1. Open `https://<domain>/download/` in a browser (any user name + the admin password).
2. Download `agent.exe` and `agent-laptop.json`. Put them in the same folder and **rename the json to `agent.json`**.
3. Double-click `agent.exe`. A console window opens and should say `connected to wss://…/agent as "laptop"`.
   The window logs every action. **Closing it stops remote control.**

Or run it from a terminal: `agent.exe -server https://<domain> -token <token> -name laptop`, and add `-read-only` to allow screenshots only.

Windows SmartScreen or antivirus may warn about an unsigned exe the first time ("More info → Run anyway").

## 3. Connect Claude

**claude.ai (web, desktop or mobile):** go to Settings → Connectors → Add custom connector and use the URL `https://<domain>/mcp`. Leave the OAuth client ID and secret empty. Click Connect, then enter the admin password on the "Allow remote control?" page.

**Claude Code:**
```bash
claude mcp add --transport http remote https://<domain>/mcp --header "Authorization: Bearer <MCP_TOKEN>"
```

Then ask: *"Take a screenshot of the Vivado window on laptop."*

## Tools

| tool | what it does |
|---|---|
| `list_machines` | connected machines, screens, monitors |
| `screenshot` | whole screen / one monitor / one window (window capture works even when the window is covered); `label` saves it for the report |
| `zoom` | full-resolution crop of a previous capture: for reading small text or cropping a figure |
| `click`, `drag`, `scroll`, `move_mouse` | coordinates are in the **last image Claude saw**; the relay maps them to real screen pixels (DPI and multi-monitor aware) |
| `type_text`, `press_keys` | Unicode typing (Cyrillic OK), combos like `ctrl+shift+s` |
| `list_windows`, `window` | find, focus, maximize, move or close windows |
| `run_command`, `job_status` | PowerShell/cmd, foreground or background jobs (e.g. `vivado -mode batch -source x.tcl`) |
| `read_file`, `write_file` | e.g. write a Tcl script, read a timing report |
| `label_capture`, `list_captures` | name figures; gallery + zip download links |

Every action that takes coordinates accepts `screenshot: true` to get a fresh picture afterwards.

## Getting the screenshots

* **Gallery:** `https://<domain>/gallery` (admin password) shows every capture.
* **Zip of labeled captures:** `https://<domain>/gallery.zip`. Files are named after their labels, e.g. `fig3_rtl_schematic.png`.
* Each capture also has a direct full-resolution link (`/c/<id>.png`) that Claude prints after taking it.

Unlabeled captures are deleted after 72 h and labeled ones after 30 days (`CAPTURE_TTL`, `LABELED_TTL`).

## Things to know on Windows

* **Black screenshots** mean the session is locked or the Remote Desktop window to it is minimized or disconnected. Keep the session unlocked. If you RDP into the PC, either keep the RDP window open or run
  `tscon %sessionname% /dest:console` (as admin) before disconnecting, so the desktop stays rendered.
* Run the agent as the **logged-in user**, not as a service. Services can't see the desktop.
* Windows blocks input into windows running **as administrator** (and UAC prompts) unless the agent is also elevated.
* Vivado is a Java app with no accessibility tree, so it's driven by screenshot + click. Anything scriptable is faster and more reliable through `run_command` with Tcl
  (`open_project`, `launch_runs`, `write_schematic`, `report_utilization`…).
* Ask before running this on university machines. It is remote-control software.

## Security

* Agents authenticate with per-machine tokens (`AGENT_TOKENS`). MCP clients use OAuth (claude.ai) or `MCP_TOKEN`.
* The OAuth consent page is protected by the admin password. Ten wrong attempts lock it for 15 minutes.
* Capture links (`/c/<id>.png`) are unguessable (96-bit random) but public to anyone who has the link.
* Anyone with the admin password or an access token can control every connected machine. Treat the password like an SSH key, and close `agent.exe` when you're done.

## Development

```bash
make test        # unit + end-to-end tests (relay + agent with a fake desktop + MCP client + OAuth flow)
make agent       # cross-compile dist/agent.exe (no CGO; builds from Linux/macOS)
make demo        # local relay on :8080 + agent with the fake "demo" desktop
```

Layout: `cmd/relay`, `cmd/agent`, `internal/relay` (hub, MCP tools, captures, OAuth), `internal/agent` (dispatch, jobs, Win32 backend, demo backend), `internal/proto` (wire protocol).
