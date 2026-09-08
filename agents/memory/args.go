package memory

import "encoding/json"

// decodeArgs reads a tool call's arguments for the approval predicate, which
// sees them before the runner validates them; false means undecodable.
func decodeArgs(argsJSON string, into any) bool {
	return json.Unmarshal([]byte(argsJSON), into) == nil
}
