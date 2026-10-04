// Package policy holds lidwake's pure decisions: when a safety cutout fires and when its latch
// clears, how the sleep block is composed and undone, which restored assertions survive a
// restart, what happens on lid close and right before the Mac goes back to sleep, how agent holds
// are keyed and bounded, what the "while the lid was closed" summary says, which requests the CLI
// socket accepts, and which socket peers the root helper trusts.
//
// Nothing here touches the system: time, process lookups, file-system checks and the sleep
// mechanisms are passed in, so every rule is unit-testable. The daemon and the helper are the
// authorities that apply these decisions; none of the types here are safe for concurrent use
// unless their documentation says so.
package policy
