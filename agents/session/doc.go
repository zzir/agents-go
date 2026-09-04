// Package session is an agent conversation's stored history: append-only
// entries forming a tree, the storage they live in, and the projection that
// turns them into model input. The runner imports it, never the reverse.
//
// The three layers: spec §2.5c. Append-only entries and the tree: spec §2.5b,
// §2.5d. Where it sits in the whole: docs/explanation/architecture.md.
package session
