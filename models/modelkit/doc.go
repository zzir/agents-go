// Package modelkit is the toolkit for writing agents.Model adapters whose
// backend does not speak the OpenAI Responses API: ParseInput walks canonical
// input into a neutral view, the item and event constructors synthesize
// canonical output whose RawJSON round-trips, and Reject fails loud on a
// feature the backend lacks. modelkit/conformancetest checks an adapter against
// the runner's contract — see docs/howto/models.md and decisions §5.10.
package modelkit
