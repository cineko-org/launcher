// Package launcher owns the portable Launcher lifecycle: static release checks,
// runtime artifact installation, Client startup, and graceful process handoff.
// It never embeds Client business behavior.
package launcher
