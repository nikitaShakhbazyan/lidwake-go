# Changelog

## 0.1.0 — 2026-10-04

First release.

- Keeps a MacBook awake with the lid closed only while AI agents work: hooks for Claude Code,
  Codex, Cursor, Gemini CLI, Aider, Cline, Hermes, OpenCode and Pi, plus an MCP server.
- `lidwake stats`: live terminal dashboard — on/off, the real lid-close state, off timer, battery,
  CPU temperature against the cutout, thermal pressure, agents, and what ran while the lid was
  closed.
- `lidwake on | off`, `lidwake timer <duration>`, `lidwake run -- <command>`, `lidwake config`.
- Restores the `disablesleep` value the Mac had before instead of forcing it off; crash, reboot
  and shutdown recovery in the root helper; a dead-man switch when the daemon disappears.
- Low-battery and thermal cutouts with the lid open too; optional AC-only mode.
- One binary for macOS 14+ (Apple Silicon and Intel): `lidwake setup` installs it without a
  Developer ID — the helper trusts only a hardened daemon running from a root-only directory.
