// Display metadata for the provider backends the server registers.
//
// Split of responsibilities with `GET /api/v1/provider-types`: the server
// serves MACHINE FACTS (which types exist, their auth modes, which request
// features fail loudly), keyed by `type`; this table holds the WORDING —
// labels, placeholders, hints — which belongs to the frontend. Adding a
// backend = one providerDef in the server registry + one entry here.

export interface ProviderTypeInfo {
  type: string;
  auth_modes?: string[];
  unsupported?: string[];
}

export interface ProviderMeta {
  /** provider_type wire value; '' is the openai default (predates the field). */
  value: string;
  /** The wire value the server reports for this entry ('' maps to 'openai'). */
  type: string;
  label: string;
  /** Short list-row badge text, colored by badgeVariant. */
  badge: string;
  /** The badge's Label variant — one color per backend (see lib/badges). */
  badgeVariant: 'accent' | 'severe';
  defaultModel: string;
  modelPlaceholder: string;
  keyPlaceholder: string;
  baseURLPlaceholder: string;
  /** Extra hint under the reasoning-effort select, when the mapping needs explaining. */
  effortHint?: string;
  /** Reasoning-effort choices this backend accepts (wire value + label). */
  effortOptions: ReadonlyArray<readonly [string, string]>;
}

const EFFORT_BASE = [
  ['', 'Not set'], ['minimal', 'Minimal'], ['low', 'Low'], ['medium', 'Medium'], ['high', 'High'],
] as const;

export const PROVIDERS: ProviderMeta[] = [
  {
    value: '',
    type: 'openai',
    label: 'OpenAI (Responses API)',
    badge: 'OpenAI',
    badgeVariant: 'accent',
    defaultModel: 'gpt-5.5',
    modelPlaceholder: 'gpt-5.5',
    keyPlaceholder: 'sk-...',
    baseURLPlaceholder: 'https://api.openai.com/v1 (leave empty for default)',
    effortOptions: [...EFFORT_BASE, ['xhigh', 'Extra High']],
  },
  {
    value: 'anthropic',
    type: 'anthropic',
    label: 'Anthropic (Messages API)',
    badge: 'Anthropic',
    badgeVariant: 'severe',
    defaultModel: 'claude-opus-5',
    modelPlaceholder: 'claude-opus-5',
    keyPlaceholder: 'sk-ant-...',
    baseURLPlaceholder: 'https://api.anthropic.com (leave empty for default)',
    effortHint: 'Maps to an Anthropic thinking budget (minimal 1k / low 4k / medium 16k / high 32k tokens)',
    effortOptions: EFFORT_BASE,
  },
];

export function providerMeta(value: string | undefined): ProviderMeta {
  // Matches the empty-default value AND the explicit type name: the API
  // accepts provider_type "openai" spelled out, so stored rows may carry
  // either form. Unknown values fall back to the default entry — the backend
  // rejects them at save/build, the form just needs something to render.
  const v = value || '';
  return PROVIDERS.find(p => p.value === v || p.type === v) ?? PROVIDERS[0];
}

/** The server-reported facts for a provider_type value, once fetched. */
export function providerFacts(infos: ProviderTypeInfo[] | null | undefined, value: string | undefined): ProviderTypeInfo | undefined {
  const type = providerMeta(value).type;
  return (infos || []).find(i => i.type === type);
}
