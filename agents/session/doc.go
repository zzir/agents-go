// Package session is an agent conversation's stored history: append-only
// entries forming a tree, the storage they live in, and the projection that
// turns them into model input.
//
// The package is the layer BELOW the runner — the runner imports it, never
// the reverse — so anything that stores, replays, inspects or repairs history
// can depend on it without pulling in the run loop. The three layers:
//
//   - Storage reads and writes entries and knows nothing about meaning.
//   - Session turns stored entries into what a model reads.
//   - Projector decides which entry kinds reach the model at all.
//
// Entries are append-only, and a session is a tree: a display that settles
// late is an update entry folded in at read time, switching attempts appends
// a leaf entry, and compaction appends a checkpoint. Nothing is rewritten,
// which is what lets a session be shared, forked, and read concurrently
// while a run is writing it.
package session
