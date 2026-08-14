// The settings panels' badge grammar. ONE semantic per color, across every
// panel — a reader who learns a color in one list must not have to relearn it
// in the next:
//
//   secondary  quiet metadata: counts, and a panel's OWN type axis (provider
//              protocol, sandbox kind, an OAuth marker). Types sit beside the
//              name that already identifies the row; color would add nothing.
//   accent     a REFERENCE to another configured entity, shown by NAME — the
//              provider an agent runs through, the agent a memory is scoped
//              to. Blue reads "this points at something you configured".
//   done       system-provided, not user-created — a built-in guardrail.
//   success / attention / danger — LIVE STATUS only, never a category, and
//              status is not a Label at all: it renders as the colored dot
//              beside the row title (form-status-dot), the way the MCP and
//              provider lists show connection/login state.
export const BADGE = {
  /** How many sub-items a row holds, always written "Thing·N" — MCP·2, Steps·3. */
  count: 'secondary',
  /** A panel's own type axis — provider protocol, sandbox kind, OAuth. */
  type: 'secondary',
  /** System-provided, not user-created — e.g. a built-in guardrail. */
  builtin: 'done',
  /** A reference to another configured entity, shown by its name. A row with
   * no reference (a global memory) carries NO badge — the default is silence. */
  ref: 'accent',
} as const;
