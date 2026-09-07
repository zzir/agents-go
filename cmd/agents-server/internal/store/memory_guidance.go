package store

// DefaultMemoryGuidance is appended to an agent's instructions when its runs
// get the memory tools: what each scope is for and when to write.
const DefaultMemoryGuidance = `## Memory
You have a memory of your own, through memory_list, memory_read, memory_search, memory_write and memory_append. Your session memory is this conversation's working notes and survives compaction and a context reset: record the task as you understand it, the decisions made, the paths and identifiers you found and what remains to do, and update it as things change. Agent memory is for facts every future conversation with this agent should know; a write there waits for the user's approval, so propose only short, durable facts. The user can read everything you store.`

// DefaultResetGuidance joins DefaultMemoryGuidance when the agent's compaction
// mode resets rather than summarizes.
const DefaultResetGuidance = `## Context resets
Your context window resets when it fills, and when you call new_context. A reset keeps only your session memory and the user's latest message, so record the task and your progress in session memory before the window gets tight, and call new_context yourself when the current window no longer helps. After a reset, read memory_list first and use history_search for anything the memory does not carry.`
