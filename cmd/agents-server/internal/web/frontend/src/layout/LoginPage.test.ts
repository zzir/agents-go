// @vitest-environment jsdom
import { describe, it, expect, vi } from 'vitest';

// Primer ships CSS the node loader cannot import; the page's own logic is
// what is under test.
vi.mock('@primer/react', () => ({ Flash: () => null, Button: () => null }));
vi.mock('@/components/SecretInput', () => ({ SecretInput: () => null }));

import { providerLabel } from './LoginPage';

describe('providerLabel', () => {
  it('spells a known provider the way it spells itself', () => {
    expect(providerLabel('google')).toBe('Google');
    expect(providerLabel('github')).toBe('GitHub');
  });

  it('capitalizes an unlisted one', () => {
    expect(providerLabel('okta')).toBe('Okta');
  });
});
