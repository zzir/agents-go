import { describe, it, expect, vi } from 'vitest';

// The panel's module pulls in Primer (CSS the node loader cannot import) and
// the API layer; flatten and pack are pure.
vi.mock('@primer/react', () => ({}));
vi.mock('@primer/react/experimental', () => ({}));
vi.mock('@/lib/hooks', () => ({ useApi: () => ({}), useCrud: () => ({}) }));
vi.mock('@/lib/api', () => ({ api: {} }));
import { flatten, pack } from '@/features/sandbox/SandboxPanel';

describe('SandboxPanel flatten / pack', () => {
  it('round-trips a remote docker row, ssh fields included', () => {
    const row = {
      id: 'sb1', name: 'lab', type: 'docker', prompt: 'python 3.12',
      config: {
        host: 'ssh://me@box', ssh_key_file: '~/.ssh/id', ssh_password: '', ssh_use_agent: true,
        ssh_known_hosts: '', ssh_insecure_host_key: false,
        image: 'img:1', runtime: 'runsc', user: 'dev', network: 'bridge',
        memory_mb: 2048, cpus: 1.5, max_read_file_bytes: 4096,
      },
    };
    const packed = pack(flatten(row));
    expect(packed).toEqual({ name: 'lab', type: 'docker', prompt: 'python 3.12', config: row.config });
  });

  // A local daemon has no SSH: those keys stay out of the config, and the
  // empty limits mean the server's defaults.
  it('packs a local docker row without ssh keys or empty limits', () => {
    const form = flatten({ name: 'local', type: 'docker', config: {} });
    expect(form.image).toBe('ghcr.io/zzir/sandbox:latest');
    const packed = pack({ ...form, ssh_use_agent: true, memory_mb: '', cpus: '0' });
    expect(packed.config).toEqual({ host: '', image: 'ghcr.io/zzir/sandbox:latest', runtime: '', user: '', network: '' });
  });

  it('round-trips an e2b row and defaults pause-on-expiry to on', () => {
    const row = {
      id: 'sb2', name: 'cloud', type: 'e2b',
      config: {
        api_url: 'https://api.example', domain: 'example.app', api_key: 'k', data_plane_auth: 'api_key',
        headers: { Authorization: 'Bearer t' },
        template_id: 'base', user: 'user', auto_pause: false, allow_internet: true, timeout_seconds: 600, max_read_file_bytes: 1024,
      },
    };
    expect(pack(flatten(row))).toEqual({ name: 'cloud', type: 'e2b', prompt: '', config: row.config });
    expect(flatten({ name: 'n', type: 'e2b', config: {} }).auto_pause).toBe(true);
    expect(flatten({ name: 'n', type: 'e2b', config: {} }).image).toBe('');
    expect(flatten({ name: 'n', type: 'e2b', config: { headers: {} } }).headers).toBe('');
  });

  it('refuses malformed headers and omits empty ones', () => {
    const base = flatten({ name: 'n', type: 'e2b', config: { template_id: 't' } });
    expect(() => pack({ ...base, headers: '{not json' })).toThrow(/valid JSON/);
    expect(() => pack({ ...base, headers: '["a"]' })).toThrow(/JSON object/);
    expect(() => pack({ ...base, headers: '{"X": 1}' })).toThrow(/string value/);
    expect(() => pack({ ...base, headers: '{"": "v"}' })).toThrow(/name/);
    expect(() => pack({ ...base, headers: '{"X": ""}' })).toThrow(/value/);
    expect(pack({ ...base, headers: ' {} ' }).config).not.toHaveProperty('headers');
  });

  it('drops a non-numeric or non-positive limit instead of sending it', () => {
    const packed = pack({ ...flatten({ name: 'n', type: 'docker', config: {} }), memory_mb: 'lots', cpus: '-1', max_read_file_bytes: '0' });
    expect(packed.config).not.toHaveProperty('memory_mb');
    expect(packed.config).not.toHaveProperty('cpus');
    expect(packed.config).not.toHaveProperty('max_read_file_bytes');
  });
});
