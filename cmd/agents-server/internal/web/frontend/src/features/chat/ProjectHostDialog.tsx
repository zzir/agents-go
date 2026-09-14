import { useEffect, useState } from 'react';
import { Dialog, Flash, Spinner, TextInput } from '@primer/react';
import type { ReactElement } from 'react';
import { api } from '@/lib/api';
import { useCopy } from '@/lib/hooks';
import type { Project } from '@/lib/binding';

/* Where a port inside the sandbox is public, on a service that publishes every
   port at <port>-<sandbox id>.<domain>: the address to copy, with <port> left
   for the reader — the port is the server's inside the sandbox, which the
   person knows and the workbench does not (decisions §5.70). */
export function ProjectHostDialog({ project, onClose }: { project: Project; onClose: () => void }): ReactElement {
  const [host, setHost] = useState<{ sandbox_id: string; domain: string } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const { copied, copy } = useCopy();

  useEffect(() => {
    let live = true;
    api.projects.sandboxHost(project.id)
      .then(h => { if (live) setHost(h); })
      .catch((e: Error) => { if (live) setError(e.message || 'Could not read the sandbox address'); });
    return () => { live = false; };
  }, [project.id]);

  const url = host ? `https://<port>-${host.sandbox_id}.${host.domain}` : '';
  return (
    <Dialog
      title={`Public URL — ${project.name}`}
      onClose={onClose}
      width="large"
      footerButtons={[
        { content: 'Close', onClick: onClose },
        { content: copied ? 'Copied' : 'Copy', buttonType: 'primary', disabled: !url, onClick: () => { void copy(url); } },
      ]}
    >
      {error && <Flash variant="danger">{error}</Flash>}
      {!error && !host && <Spinner size="small" />}
      {host && (
        <>
          <TextInput block monospace readOnly value={url} aria-label="Public URL" onFocus={e => e.target.select()} />
          <p className="env-editor-hint">
            A server listening on a port inside the sandbox answers at this address with <code>&lt;port&gt;</code> replaced
            by that port — while the sandbox is running.
          </p>
        </>
      )}
    </Dialog>
  );
}
