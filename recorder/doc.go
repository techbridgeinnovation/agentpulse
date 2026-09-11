// Package recorder records what an agent spent, from inside the agent's own
// process.
//
// Two lines wire it in. Everything after that is deliberately invisible: a
// record is handed to a bounded queue and a background worker delivers it to
// whichever sinks were configured.
//
// The first rule of this package is that it must never degrade its host. A
// full queue drops records rather than blocking a turn, a sink that fails or
// panics is isolated from the others and from the caller, and nothing here
// returns an error a caller has to handle. That ranks above completeness of
// data: telemetry that can break production gets removed, and rightly.
//
// Dropped records are counted and the count is readable. That counter is the
// one thing this package will not let you turn off — silent loss is worse than
// visible loss, and an agent whose records are being dropped needs to be able
// to find out.
//
// What each in-flight request has spent is kept here too, so a spend ceiling is
// readable before a model call without a network call to ask. It is priced
// against a rate card fetched in the background, and a card that is stale or
// absent prices at nothing rather than delaying anything. The map holding those
// totals expires entries and has a hard cap, so a caller who never releases a
// request costs a bounded amount of memory.
package recorder
