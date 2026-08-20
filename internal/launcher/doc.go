// Package launcher owns the portable Launcher lifecycle: Central health and
// authentication, runtime artifact installation, Client startup, and graceful
// process handoff. It never embeds Client business behavior.
package launcher
