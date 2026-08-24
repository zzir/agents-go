// The settings panels' badge grammar. ONE semantic per color, across every
// panel — a reader who learns a color in one list must not have to relearn it
// in the next:
//
//   secondary  quiet metadata: counts, and a panel's OWN type axis (sandbox
//              kind, MCP transport). Types sit beside the name that already
//              identifies the row; color would add nothing. The one exception
//              is the provider-type badge, which carries a per-backend color
//              (ProviderMeta.badgeVariant) so mixed-backend lists scan.
//   accent     a REFERENCE to another configured entity, shown by NAME — the
//              provider an agent runs through, the agent a memory is scoped
//              to. Blue reads "this points at something you configured".
//   done       published or system-provided: a scoped row's Global/Private
//              badge, and a built-in guardrail. The two never meet on one
//              screen — the guardrails panel has no scoped rows — so purple
//              stays unambiguous within any one list.
//   success / attention / danger — LIVE STATUS only, never a category, and
//              status is not a Label at all: it renders as the colored dot
//              beside the row title (form-status-dot), the way the MCP and
//              provider lists show connection/login state.
// Badge ORDER on a row is fixed too: scope first (whose row is this), then
// the type/reference axis (what it runs on), counts last — semantics from
// ownership down to metadata, color weight following the same gradient.
export const BADGE = {
  /** How many sub-items a row holds, always written "Thing·N" — MCP·2, Steps·3. */
  count: 'secondary',
  /** A panel's own type axis — sandbox kind, MCP transport, OAuth. */
  type: 'secondary',
  /** System-provided, not user-created — e.g. a built-in guardrail. */
  builtin: 'done',
  /** A reference to another configured entity, shown by its name. A row with
   * no reference (a global memory) carries NO badge — the default is silence. */
  ref: 'accent',
  /** A scoped row's visibility: Global, or a foreign Private row in the
   * admin's cross-user view. An own private row carries NO badge. */
  scope: 'done',
} as const;
