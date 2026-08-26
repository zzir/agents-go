// Package modelkit is the toolkit for writing agents.Model adapters whose
// backend does not speak the OpenAI Responses API.
//
// The SDK's canonical item and event format IS the Responses wire format
// (decisions §5.5): session entries, run state and every exported signature carry
// Responses items, and the runner consumes a fixed set of output item types
// and stream events (decisions §5.10). An adapter for another protocol therefore
// has exactly one job — translate its wire format into canonical Responses
// bytes at the model boundary, in both directions. This package holds the
// shared halves of that job so each adapter only writes the vendor-specific
// part:
//
//   - [ParseInput] walks canonical input items into a neutral view an adapter
//     can map onto its own request types, without re-implementing the
//     string-or-parts content quirks of the Responses format.
//   - [MessageItem], [FunctionCallItem], [ReasoningItem] and the event
//     constructors ([OutputItemDoneEvent], [CompletedEvent], …) synthesize
//     canonical output items and stream events whose RawJSON round-trips —
//     the property agents.OutputToInput and session persistence rely on.
//   - [Reject] enforces the fail-loud contract for request features the
//     backend has no equivalent for: a *agents.UserError naming the feature,
//     never a silently dropped setting.
//
// The conformance suite in modelkit/conformancetest checks an adapter against
// the runner's consumption contract; every Model implementation in this
// repository must pass it.
package modelkit
