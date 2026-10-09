// Package session is an agent conversation's stored history: append-only
// entries forming a tree, the storage they live in, and the projection that
// turns them into model input. The runner imports it, never the reverse —
// spec §2.5b–§2.5d, decisions §5.17.
package session
