# lidwake

Keep your MacBook awake with the lid closed — **only while your AI agents are working** — and
watch it from the terminal.

```text
lidwake  ● ON · keeping the Mac awake                                   23:32:05
────────────────────────────────────────────────────────────────────────────────
  Lid-close sleep  blocked — closing the lid won't sleep the Mac
  Lid              closed
  Off timer        55m 17s  until 00:27
  Battery          ▕██████┃██████████████░░░░░░░░▏  71% on battery · stop at 20%
  CPU temp         ▕███████████████░░░░░░┃░░░░░░░▏  67°C warm
                   30°              80° cutout
  Thermal          ○ nominal  ● fair  ○ serious  ○ critical
────────────────────────────────────────────────────────────────────────────────
  AGENTS  3 keeping the Mac awake
  claude-code   pid 48211  working  41m
  codex         pid 48907  waiting: permission  7m
  make          pid 49120  hold  1m, 3h 56m left  lidwake run make test
────────────────────────────────────────────────────────────────────────────────
  [space] on/off  [t] timer  [+/-] ±15m  [r] release all  [q] quit
```

## Why

Close the lid of a MacBook and it sleeps — and so does the agent you left running.
`caffeinate` and every other power assertion only prevent *idle* sleep; the lid is a direct order
that outranks them. The one switch that overrides it is `pmset disablesleep 1`, and it is a
foot-gun:

- it is global and it **survives reboots** (it is stored in the power-management preferences);
- it outlives the process that set it — crash, and the Mac never sleeps again;
- while it is set, the kernel refuses even its **own emergency sleep** for overheating
  (`IOPMrootDomain::checkSystemSleepAllowed` in XNU), whatever the lid does.

lidwake flips that switch only while an agent is actually mid-task, puts it back the moment the
work ends, and keeps a safety net under it the whole time.

## Features

- **Agent-aware.** Hooks for Claude Code, Codex, Cursor, Gemini CLI, Aider, Cline, Hermes, OpenCode
  and Pi keep the Mac awake only while a turn is running; an idle session at the prompt lets it
  sleep. Process-exit and CPU-idle sweeps catch interrupted turns.
- **Live dashboard.** `lidwake stats` shows on/off, the real lid-close state, an off timer,
  battery, CPU temperature on a normal → critical scale, macOS thermal pressure and every agent
  holding the Mac awake — plus what ran the last time the lid was closed.
- **Off timer.** `lidwake timer 1h` (or `t` in the dashboard): lidwake turns itself off at the
  deadline — even if the terminal is gone, the daemon restarted or the Mac slept in between.
- **Wrap any command.** `lidwake run -- ./long-job.sh` keeps the Mac awake exactly as long as the
  command runs and passes its exit code through.
- **Safety cutouts, lid open or closed.** Low battery (default 20%) and CPU temperature (default
  80 °C) release every hold so macOS can sleep. Optional **AC-only** mode. A cutout latches until
  the hazard recedes, so a busy agent can't immediately re-pin a hot or flat Mac.
- **Your own setting survives.** If you had `disablesleep` on before (a Mac used as a server),
  lidwake restores *that* value instead of forcing sleep back on.
- **Crash-proof.** The root helper restores the saved value at boot, on shutdown and a minute
  after the daemon disappears; the daemon re-applies the block within seconds if the helper
  restarts.
- **One binary, macOS 14+, no Apple Developer account.** Apple Silicon and Intel.

## Install

Step-by-step guide with checks and troubleshooting: [docs/INSTALL.md](docs/INSTALL.md) ·
по-русски: [docs/INSTALL.ru.md](docs/INSTALL.ru.md).

**Prebuilt** (universal binary; checks the SHA-256, then asks for your password once for the root
helper):

```sh
curl -fsSL https://raw.githubusercontent.com/nikitaShakhbazyan/lidwake-go/main/install.sh | bash
```

**With Go** 1.26+ (needs the Xcode Command Line Tools for cgo):

```sh
go install github.com/nikitaShakhbazyan/lidwake-go/cmd/lidwake@latest
~/go/bin/lidwake setup
```

**From source:**

```sh
git clone https://github.com/nikitaShakhbazyan/lidwake-go.git && cd lidwake-go
make install
```

`lidwake setup` copies the binary to `/usr/local/libexec/lidwake/` (root-owned), links
`/usr/local/bin/lidwake`, and registers the root helper and your per-user daemon with launchd.
Then wire it into your agents and watch:

```sh
lidwake install-hooks   # every agent it finds; --tool claude-code for one, --dry-run to preview
lidwake stats
```

## Usage

```text
lidwake stats [--once]                 live dashboard; --once prints one frame
lidwake on | off                       let agents keep the Mac awake, or stop and release all
lidwake timer <duration> | off         turn lidwake off later: 30m, 1h, 1h30m
lidwake run [--for <d>] -- <command>   keep the Mac awake while <command> runs
lidwake hold [--for <d>] [--pid <n>]   keep it awake for a background job; prints a hold id
lidwake release <id> | --all           end a hold, or everything
lidwake config [<key> [<value>]]       show or change a setting
lidwake install-hooks | uninstall-hooks [--tool <name>] [--dry-run]
lidwake setup | uninstall              install/upgrade, or remove everything (keeps settings)
lidwake status [--json] | daemon-status | mcp | version
```

Agents that speak MCP can place holds themselves through `lidwake mcp`.

### Settings

`lidwake config` lists everything; the daemon picks changes up immediately.

| Key | Default | |
|---|---|---|
| `lowBatteryThresholdPercent` | `20` | release all holds at or below this charge on battery |
| `thermalThresholdCelsius` | `80` | release all holds at this CPU temperature |
| `safetyCutoutsWithLidOpen` | `true` | run both cutouts with the lid open too |
| `requireACPower` | `false` | keep the Mac awake on AC power only |
| `manualHoldMaxHours` | `4` | cap for `hold` and `run` |
| `idleReleaseSeconds` | `90` | drop a hold whose agent has been CPU-idle this long |
| `lockOnLidClose` | `true` | lock the screen when the lid closes over a working agent |
| `agentWaitingPolicy` | `grace` | while an agent waits for you: `keepAwake`, `grace` or `sleep` |

## How it works

One binary in three roles:

```text
lidwake (CLI) ──unix socket──▶ lidwake daemon (you, LaunchAgent) ──unix socket──▶ lidwake helper (root, LaunchDaemon)
  hooks, stats, run              holds, timer, cutouts, lid/battery/        set(blocked) only:
                                 temperature monitors — all policy          pmset disablesleep + idle assertion
```

The helper is the only privileged code and holds no policy. It identifies each caller by its
audit token and trusts it only if its executable is `/usr/local/libexec/lidwake/lidwake` — a path
only root can write, checked up to `/` — signed with the hardened runtime. That is what makes an
ad-hoc, built-from-source install safe without a Developer ID. Details:
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Things to know

- **Heat.** A closed MacBook under sustained load in a bag gets hot. The thermal cutout is a net,
  not a cooling system — leave it somewhere with air.
- **Network.** Wi-Fi stays up with the lid closed, so cloud agents keep talking to their APIs.
- `pmset -g` lists the flag as `SleepDisabled`, and only once it has been set at least once.
- Logs: `~/Library/Logs/lidwake/daemon.log`, `/var/log/lidwake-helper.log`; `lidwake status`
  and `lidwake daemon-status` for a quick look.

## Uninstall

```sh
lidwake uninstall   # removes the hooks, the daemon, the helper and the binary
```

Settings stay in `~/Library/Application Support/lidwake`.

## Development

```sh
make test    # go test -race ./... — no root needed, nothing touches the real system
make build   # bin/lidwake
```

Layout and design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## License

[MIT](LICENSE).
