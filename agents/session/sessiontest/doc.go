// Package sessiontest holds the conformance suites a session backend runs:
// [StorageConformance] for a [session.Storage], [RepoConformance] for a
// [session.Repo]. They are what the SDK's own backends pass; a backend of your
// own runs them from its tests — spec §2.5e2.
package sessiontest
