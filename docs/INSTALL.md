# Installing lidwake

[По-русски](INSTALL.ru.md)

## Requirements

- macOS 14 Sonoma or newer, Apple Silicon or Intel.
- An administrator account: setup asks for your password once, to install the root helper.
- Only to build it yourself: Go 1.26+ and the Xcode Command Line Tools (`xcode-select --install`).

## 1. Install

Pick one.

### Prebuilt binary (recommended)

```sh
curl -fsSL https://raw.githubusercontent.com/nikitaShakhbazyan/lidwake-go/main/install.sh | bash
```

The script downloads the latest [release](https://github.com/nikitaShakhbazyan/lidwake-go/releases)
(one universal binary for Apple Silicon and Intel), checks its SHA-256 against the release's
`checksums.txt` and runs `lidwake setup`. To pin a version:
`curl -fsSL …/install.sh | LIDWAKE_VERSION=0.1.0 bash`.

### With Go

```sh
go install github.com/nikitaShakhbazyan/lidwake-go/cmd/lidwake@latest
"$(go env GOPATH)/bin/lidwake" setup
```

### From source

```sh
git clone https://github.com/nikitaShakhbazyan/lidwake-go.git
cd lidwake-go
make install
```

## 2. What `lidwake setup` does

It asks for your password first, then:

1. stops a running lidwake daemon, if there is one (an upgrade);
2. copies the binary to `/usr/local/libexec/lidwake/lidwake`, owned by root, and signs it ad hoc
   with the hardened runtime (release binaries come signed already);
3. links `/usr/local/bin/lidwake` to it;
4. checks that every directory on that path is writable by root alone — the helper trusts nothing
   else — and warns if one isn't;
5. registers and starts the root helper:
   `/Library/LaunchDaemons/io.github.nikitashakhbazyan.lidwake.helper.plist`;
6. registers and starts your daemon:
   `~/Library/LaunchAgents/io.github.nikitashakhbazyan.lidwake.daemon.plist`;
7. repairs lidwake hooks already in your agents' configs that point at an older binary.

Running it again upgrades in place. macOS may show a *Background Items Added* notification for the
helper and the daemon; that's expected.

## 3. Connect your agents

```sh
lidwake install-hooks --dry-run   # preview the changes
lidwake install-hooks             # every agent it finds
lidwake install-hooks --tool claude-code
```

Agents: `claude-code`, `codex`, `cursor`, `gemini-cli`, `aider`, `cline`, `hermes`, `opencode`,
`pi`. Only lidwake's own entries are added (tagged `_lidwake`); the rest of each config is kept.

- **Codex** runs a hook only after you trust it: open Codex and approve the lidwake hooks in
  `/hooks`.
- Restart agent sessions that were already open, so they load the hooks.
- Not using hooks? `lidwake run -- <command>` keeps the Mac awake while any command runs.

## 4. Check that it works

```sh
lidwake daemon-status    # daemon: running
lidwake stats            # no "privileged helper is not connected" warning
```

A real test, with the lid:

```sh
lidwake run -- sleep 300       # terminal 1: holds the Mac awake for 5 minutes
pmset -g | grep SleepDisabled  # terminal 2: SleepDisabled 1 while it runs
```

Close the lid for a minute and open it again: `lidwake stats` still shows the hold, and
`pmset -g log | grep "Entering Sleep"` has no new entry. When `sleep 300` ends, `SleepDisabled`
goes back to what it was (`0`, unless you had set it yourself).

## 5. Everyday use

```sh
lidwake stats          # live dashboard: space on/off, t timer, r release all, q quit
lidwake off | on       # stop keeping the Mac awake, or allow it again
lidwake timer 1h       # turn off by itself in an hour
lidwake config         # settings: battery floor, thermal cutout, AC-only…
```

See the [README](../README.md) for every command and setting.

## 6. Update

Run the same install command again. Setup replaces the binary, restarts the helper and the daemon,
and keeps your settings and hooks.

## 7. Uninstall

```sh
lidwake uninstall
```

It asks for your password first, then removes the hooks from every agent, the daemon, the helper
and the binary, and restores the sleep setting. Your settings and logs stay; to remove them too:

```sh
rm -rf ~/Library/Application\ Support/lidwake ~/Library/Logs/lidwake
```

## Troubleshooting

**`lidwake: command not found`** — open a new terminal. `/usr/local/bin` is on the default macOS
`PATH`; if yours replaces it, add it back.

**"The privileged helper is not connected"** — check the helper with
`sudo launchctl print system/io.github.nikitashakhbazyan.lidwake.helper | head` and its log,
`/var/log/lidwake-helper.log`, then run `lidwake setup` again.

**Setup warns "… the helper will not trust the daemon"** — a directory on
`/usr/local/libexec/lidwake/lidwake`, often `/usr/local` or `/usr/local/libexec` after an old
Homebrew install, is writable by a regular user. Make it root's:
`sudo chown root:wheel <dir> && sudo chmod go-w <dir>`, then run `lidwake setup` again. On an
Intel Mac whose Homebrew lives in `/usr/local`, change only the directory the warning names.

**macOS refuses to open a binary you downloaded in a browser** — remove the quarantine flag:
`xattr -d com.apple.quarantine ./lidwake`. The curl installer is not affected.

**Setup fails at "sign the binary"** — signing needs the Xcode Command Line Tools
(`xcode-select --install`). Release binaries are signed already, so the prebuilt install never
signs.

**The Mac still sleeps with the lid closed** — open `lidwake stats`:
- *OFF* → `lidwake on`;
- no agents listed → the hooks aren't installed (step 3) or the session started before them;
- *CUTOUT* → a safety cutout fired: battery at or below the floor, AC-only mode on battery, or the
  CPU too hot; it lifts once the condition clears;
- a helper warning → see above.

**The Mac doesn't sleep after the work is done** — `lidwake status` lists what still holds it;
`lidwake release --all` ends everything. Last resort: `sudo pmset -a disablesleep 0`.

**Logs** — `~/Library/Logs/lidwake/daemon.log`, `/var/log/lidwake-helper.log`, and the event log
`~/Library/Application Support/lidwake/events.log`.

> A closed MacBook under sustained load in a bag gets hot. The thermal cutout is a safety net, not
> a cooling system — leave it somewhere with air.
