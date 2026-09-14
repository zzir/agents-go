import { Link } from '@primer/react';

// composerGate says whether the composer can send at all, from the agent list
// as useApi delivers it: null is a list not in hand (loading, or failed), an
// empty array is a workbench with no agent — the one case the first-mile card
// answers. `blocked` is the placeholder the disabled textarea shows.
export type ComposerGate =
  | { state: 'ready'; blocked?: undefined }
  | { state: 'loading' | 'error' | 'none'; blocked: string };

export function composerGate(agentConfigs: { id: string }[] | null, error: string | null): ComposerGate {
  if (agentConfigs === null) {
    return error
      ? { state: 'error', blocked: 'Agents could not be loaded — reload to retry' }
      : { state: 'loading', blocked: 'Loading agents…' };
  }
  if (agentConfigs.length === 0) return { state: 'none', blocked: 'Add a provider and an agent in Settings to start' };
  return { state: 'ready' };
}

const STEPS: { tab: string; title: string; detail: string }[] = [
  { tab: 'providers', title: 'Add a provider', detail: 'an endpoint and its API key, or a ChatGPT sign-in' },
  { tab: 'agents', title: 'Create an agent', detail: 'a model on that endpoint, with its instructions' },
  { tab: '', title: 'Send a message', detail: 'the first message makes the session' },
];

// FirstMileCard replaces the greeting on a workbench with no agent: the three
// steps to the first reply, each a link into the Settings tab that does it.
export function FirstMileCard({ onSettingsOpen }: { onSettingsOpen?: (tab?: string) => void }) {
  return (
    <div className="first-mile" role="region" aria-label="Getting started">
      <div className="first-mile-title">Three steps to a first reply</div>
      <ol className="first-mile-steps">
        {STEPS.map((s, i) => (
          <li key={s.tab || i} className="first-mile-step">
            {s.tab && onSettingsOpen
              ? <Link as="button" type="button" className="first-mile-link" onClick={() => onSettingsOpen(s.tab)}>{s.title}</Link>
              : <span className="first-mile-link">{s.title}</span>}
            <span className="first-mile-detail">{s.detail}</span>
          </li>
        ))}
      </ol>
    </div>
  );
}
