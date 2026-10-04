# Architecture

## One binary, three roles

| Role | Command | Runs as | Started by | Job |
|---|---|---|---|---|
| CLI | `lidwake …` | you, agent hooks | you | `stats`, `on`/`off`, `timer`, `run`, `hold`, hooks, MCP server, `setup` |
| Daemon | `lidwake daemon` | you | `~/Library/LaunchAgents/io.github.nikitashakhbazyan.lidwake.daemon.plist` | all policy |
| Helper | `lidwake helper` | root | `/Library/LaunchDaemons/io.github.nikitashakhbazyan.lidwake.helper.plist` | flips the sleep block, nothing else |

`lidwake setup` installs the binary at `/usr/local/libexec/lidwake/lidwake` (root-owned, ad-hoc
signed with the hardened runtime) and links `/usr/local/bin/lidwake` to it.

- **CLI → daemon:** a Unix socket at `~/Library/Application Support/lidwake/cli.sock` (0600).
  Frames are a 4-byte big-endian length plus JSON. Ops: `ping`, `status`, `acquire`, `release`,
  `hold`, `releaseAll`, `pause`, `resume`, `timer`, `reloadSettings`.
- **Daemon → helper:** a Unix socket at `/var/run/lidwake/helper.sock`, same framing, one
  persistent connection. Ops: `set {blocked}`, `state`, `version`.

## Blocking sleep

Two mechanisms, both held by the helper:

1. an IOPMAssertion (`PreventUserIdleSystemSleep`) for idle sleep, which the kernel drops if the
   helper dies;
2. `pmset -a disablesleep 1` for lid-close sleep. Public assertions — and so `caffeinate` — never
   override a closed lid, and the in-process IOKit routes to `SleepDisabled` either fail as root
   or don't stick. `/usr/bin/pmset` runs under a 10 s watchdog.

`disablesleep` lives in the power-management preferences, so it outlives the helper and survives a
reboot. The helper's policy (`internal/policy.SleepBlockPolicy`):

- saves the current value to `/var/db/lidwake/sleep-disabled-before` (root-only) before the first
  block and restores *that* value on release — a Mac its owner set never to sleep stays so;
- restores a saved value when the helper starts — at boot (`RunAtLoad`) or after a crash — and
  leaves the flag alone when nothing was saved;
- re-applies the block on every repeated `set(true)`; the daemon repeats it every 60 s and after a
  wake.

The helper keeps one request per user, identified by the caller's UID: the block holds while any
user's daemon wants it, and another user's daemon can neither clear nor prolong yours. It restores
the setting on SIGTERM (shutdown, uninstall) and, through a per-user dead-man switch, when a
blocking daemon's connection has been gone for 60 s and nobody else wants the block; a restore that
fails is retried. The daemon, in turn, checks every 5 s while blocking that the helper still holds
the block and re-applies it at once after a helper restart.

## Trusting the daemon without a Developer ID

Only the daemon may drive the helper. A Developer-ID build anchors that in the team identifier,
but a built-from-source install is ad-hoc signed, and an ad-hoc binary can claim any identifier.
The helper resolves each peer from its audit token (`LOCAL_PEERTOKEN`, not the PID) and trusts it
only when:

- its executable is exactly `/usr/local/libexec/lidwake/lidwake`, and that file and every directory
  up to `/` are owned by root and not group- or world-writable — so only root could have put it
  there;
- it runs with the hardened runtime, so a user-editable LaunchAgent plist can't inject a library
  through `DYLD_INSERT_LIBRARIES`;
- its code passes `SecCodeCheckValidity`.

## Knowing when agents work

- **Hooks** (`lidwake install-hooks`) for Claude Code, Codex, Cursor, Gemini CLI, Aider, Cline,
  Hermes, OpenCode and Pi call `lidwake acquire` when a turn starts and `lidwake release` when it
  ends. Entries are tagged `_lidwake`, so uninstalling removes exactly what was added.
- **Process exit:** kqueue `NOTE_EXIT` on the owning PID releases a hold the moment its process
  dies; `run` and `hold --pid` rely on it.
- **CPU-idle sweep:** a hold whose process tree stays under ~3% of a core for
  `idleReleaseSeconds` is dropped — the catch for an interrupted turn that fired no end hook. A
  tree holding its own `caffeinate -i` (Claude Code does while it thinks) counts as working.
- **Waiting for the user:** Claude Code's session status shows when it stopped mid-turn for an
  answer; `agentWaitingPolicy` decides whether that keeps the Mac awake.
- **Process sniffing** (opt-in): auto-acquire for a known agent running without hooks.

Holds are reference-counted by key; the Mac is blocked while at least one exists.

## Safety cutouts

- **Thermal:** CPU temperature from the SMC (`Tp…`/`Te…` sensors on Apple Silicon, `TC0P`… on
  Intel) at or above `thermalThresholdCelsius`.
- **Low battery:** on battery at or below `lowBatteryThresholdPercent`.
- **AC only** (`requireACPower`): any battery power.

A cutout releases every hold and latches: acquires are refused until the hazard recedes with
margin (5 °C cooler, 5% more charge, or AC power). With `safetyCutoutsWithLidOpen` (the default)
the cutouts run whatever the lid does, because `SleepDisabled` blocks the kernel's own emergency
sleep with the lid open too. Switching a cutout off drops its latch.

## Off timer

`lidwake timer` stores a deadline in the daemon, persisted in `state.json`. At the deadline the
daemon pauses: every hold is released and acquires are ignored until `lidwake on`. A deadline that
passed while the Mac slept or the daemon was down fires at once.

## Files

`~/Library/Application Support/lidwake/`: `config.json` (settings), `state.json` (holds, paused,
off timer), `events.log`, `cli.sock`. Logs: `~/Library/Logs/lidwake/daemon.log`,
`/var/log/lidwake-helper.log`.

## Source layout

| Package | |
|---|---|
| `cmd/lidwake` | entry point |
| `internal/cli`, `internal/mcp`, `internal/install` | commands, MCP server, setup/uninstall |
| `internal/daemon`, `internal/monitor`, `internal/store` | policy engine, monitors, persistence |
| `internal/helper` | the root helper |
| `internal/policy`, `internal/activity` | pure decision logic |
| `internal/registry`, `internal/agents`, `internal/hooks` | holds, agent detection, hook installers |
| `internal/tui`, `internal/chime` | dashboard, sounds |
| `internal/darwin` | every macOS call (cgo) |
| `internal/ipc`, `internal/model`, `internal/settings`, `internal/paths` | wire protocol, shared types |
