// Package modelkit is the toolkit for writing agents.Model adapters whose
// backend does not speak the OpenAI Responses API: ParseInput walks canonical
// input, the item and event constructors synthesize canonical output, and
// Reject fails loud on a missing feature — see docs/howto/models.md, decisions §5.10.
package modelkit
