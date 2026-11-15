/**
 * pages/ProjectDetailPage.tsx -- one project: its keys, its sheets, suspend / resume, revoke.
 *
 *   read        GET    /admin/projects/{id}             viewer  internal/api/tenancy.go getProject
 *   issue key   POST   /admin/projects/{id}/keys        admin   tenancy.go issueKey   -> shown once
 *   rotate key  POST   /admin/keys/{id}/rotate          admin   tenancy.go rotateKey  -> shown once
 *   revoke key  DELETE /admin/keys/{id}                 admin   tenancy.go revokeKey
 *   new sheet   POST   /admin/projects/{id}/sheets      admin   api.go createSheet
 *   suspend     POST   /admin/projects/{id}/suspend     admin   tenancy.go suspendProject
 *   resume      POST   /admin/projects/{id}/resume      admin   tenancy.go resumeProject
 *   revoke      POST   /admin/projects/{id}/revoke      admin   revoke.go revokeProject (name typed)
 *
 * Sheet collaborators are the sessions on the node that answered (the server's own note on
 * SheetInfo); the Sessions page merges both nodes.
 */

import React, { useCallback, useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { api, seg } from '@/api/client';
import type { IssuedKey, KeyInfo, ProjectDetail, SheetInfo } from '@/api/types';
import { atLeast, useAuth } from '@/auth/AuthContext';
import {
  Badge,
  Button,
  DataTable,
  DateCell,
  EmptyState,
  GlassCard,
  Input,
  Mono,
  Muted,
  Primary,
  RowMenu,
  Skeleton,
  useModal,
  type Column,
  type UseModal,
} from '@/features/ui';
import { showSecret } from '@/features/ui/SecretDialog';
import { useToast } from '@/features/ui/Toast';
import { ENV_KEY } from './ProjectsPage';

const RevokeBody: React.FC<{ name: string; onConfirm: () => Promise<void>; close: () => void }> = ({ name, onConfirm, close }) => {
  const [typed, setTyped] = useState('');
  const [busy, setBusy] = useState(false);
  return (
    <div className="space-y-4">
      <p className="text-sm text-slate-600 dark:text-slate-300">
        Every key of <Primary>{name}</Primary> is revoked and every sheet and its entries are dropped. This cannot be undone.
        Type the project name to confirm.
      </p>
      <Input label="Project name" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button
          variant="primary"
          disabled={typed !== name || busy}
          onClick={async () => {
            setBusy(true);
            try {
              await onConfirm();
              close();
            } finally {
              setBusy(false);
            }
          }}
        >
          Revoke project
        </Button>
      </div>
    </div>
  );
};

function confirmRevoke(modal: UseModal, name: string, onConfirm: () => Promise<void>): void {
  modal.open({ title: 'Revoke project', size: 'md', body: (close) => <RevokeBody name={name} onConfirm={onConfirm} close={close} /> });
}

export const ProjectDetailPage: React.FC = () => {
  const { id = '' } = useParams();
  const { me } = useAuth();
  const canWrite = atLeast(me?.role, 'admin');
  const navigate = useNavigate();
  const modal = useModal();
  const toast = useToast();
  const [p, setP] = useState<ProjectDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [keyLabel, setKeyLabel] = useState('');
  const [sheetName, setSheetName] = useState('');
  const [sheetPassword, setSheetPassword] = useState('');

  const load = useCallback(() => {
    api<ProjectDetail>('GET', `/admin/projects/${seg(id)}`)
      .then((d) => (setP(d), setError(null)))
      .catch((e: Error) => setError(e.message));
  }, [id]);
  useEffect(load, [load]);

  const act = async (fn: () => Promise<unknown>, done?: string) => {
    try {
      await fn();
      if (done) toast.show(done, 'success');
      load();
    } catch (err) {
      toast.fail(err);
    }
  };

  const showKey = (k: IssuedKey, title: string) =>
    showSecret(modal, {
      title,
      label: 'API key',
      secret: k.api_key,
      envLine: `${ENV_KEY}=${k.api_key}`,
      note: (
        <>
          Key prefix <Mono>{k.key_prefix}</Mono>
          {k.label ? <> -- label {k.label}</> : null}.
        </>
      ),
    });

  if (error && !p) return <EmptyState variant="error" title="Project not loaded" description={error} />;
  if (!p) return <Skeleton height="10rem" />;

  const keyColumns: Column<KeyInfo>[] = [
    { key: 'prefix', header: 'Prefix', render: (k) => <Mono>{k.key_prefix}</Mono> },
    { key: 'label', header: 'Label', render: (k) => (k.label ? k.label : <Muted>--</Muted>) },
    {
      key: 'state',
      header: 'State',
      render: (k) => (k.revoked_at ? <Badge tone="error">Revoked</Badge> : <Badge tone="success">Live</Badge>),
    },
    { key: 'created', header: 'Created', render: (k) => <DateCell iso={k.created_at} /> },
    { key: 'used', header: 'Last used', render: (k) => <DateCell iso={k.last_used_at} /> },
  ];
  if (canWrite) {
    keyColumns.push({
      key: 'actions',
      header: '',
      align: 'right',
      render: (k) =>
        k.revoked_at ? null : (
          <RowMenu
            label={`Actions for key ${k.key_prefix}`}
            items={[
              {
                id: 'rotate',
                label: 'Rotate',
                onClick: () =>
                  act(async () => showKey(await api<IssuedKey>('POST', `/admin/keys/${seg(k.key_id)}/rotate`), 'Key rotated')),
              },
              {
                id: 'revoke',
                label: 'Revoke',
                variant: 'danger',
                onClick: () => act(() => api('DELETE', `/admin/keys/${seg(k.key_id)}`), `Key ${k.key_prefix} revoked`),
              },
            ]}
          />
        ),
    });
  }

  const sheetColumns: Column<SheetInfo>[] = [
    { key: 'name', header: 'Sheet', render: (s) => <Primary>{s.name}</Primary> },
    { key: 'entries', header: 'Entries', align: 'right', render: (s) => s.keys },
    { key: 'collab', header: 'Sessions (this node)', align: 'right', render: (s) => s.collaborators },
  ];

  const suspended = !!p.suspended_at;

  return (
    <div className="space-y-6">
      <GlassCard
        title={p.name}
        subtitle={<Mono>{p.project_id}</Mono>}
        actions={
          canWrite ? (
            <>
              <Button
                onClick={() =>
                  act(
                    () => api('POST', `/admin/projects/${seg(p.project_id)}/${suspended ? 'resume' : 'suspend'}`),
                    suspended ? 'Project resumed' : 'Project suspended',
                  )
                }
              >
                {suspended ? 'Resume' : 'Suspend'}
              </Button>
              <Button
                variant="ghost"
                className="text-[var(--color-error)]"
                onClick={() =>
                  confirmRevoke(modal, p.name, async () => {
                    try {
                      await api('POST', `/admin/projects/${seg(p.project_id)}/revoke`);
                      toast.show(`Project ${p.name} revoked`, 'success');
                      navigate('/projects', { replace: true });
                    } catch (err) {
                      toast.fail(err);
                    }
                  })
                }
              >
                Revoke project
              </Button>
            </>
          ) : null
        }
      >
        <div className="flex flex-wrap gap-6 text-sm">
          <span>
            {suspended ? <Badge tone="warning">Suspended</Badge> : <Badge tone="success">Active</Badge>}
          </span>
          <Muted>Created {new Date(p.created_at).toLocaleString()}</Muted>
          {suspended && <Muted>Suspended {new Date(p.suspended_at!).toLocaleString()}</Muted>}
          <Muted>{p.keys} live keys</Muted>
          <Muted>{p.sheets} sheets</Muted>
        </div>
      </GlassCard>

      <GlassCard
        title="API keys"
        subtitle="A key is shown once, when it is issued or rotated."
        flush
        actions={
          canWrite ? (
            <form
              className="flex items-end gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                act(async () => {
                  const k = await api<IssuedKey>('POST', `/admin/projects/${seg(p.project_id)}/keys`, keyLabel ? { label: keyLabel } : {});
                  setKeyLabel('');
                  showKey(k, 'Key issued');
                });
              }}
            >
              <input
                aria-label="New key label (optional)"
                placeholder="Label (optional)"
                maxLength={200}
                value={keyLabel}
                onChange={(e) => setKeyLabel(e.target.value)}
                className="rounded-[var(--admin-radius-control)] border border-slate-300 bg-white/50 px-3 py-1.5 text-sm text-slate-800 dark:border-slate-600 dark:bg-slate-900/50 dark:text-slate-100"
              />
              <Button type="submit" variant="primary" size="sm">
                Issue key
              </Button>
            </form>
          ) : null
        }
      >
        <DataTable caption="API keys" columns={keyColumns} rows={p.api_keys} rowKey={(k) => k.key_id} emptyTitle="No keys" />
      </GlassCard>

      <GlassCard title="Sheets" flush>
        {canWrite && (
          <form
            className="flex flex-col gap-4 border-b border-white/10 px-6 py-4 md:flex-row md:items-end"
            onSubmit={(e) => {
              e.preventDefault();
              act(async () => {
                await api('POST', `/admin/projects/${seg(p.project_id)}/sheets`, { name: sheetName, password: sheetPassword });
                setSheetName('');
                setSheetPassword('');
              }, `Sheet ${sheetName} created`);
            }}
          >
            <Input label="Sheet name" required value={sheetName} onChange={(e) => setSheetName(e.target.value)} />
            <Input
              label="Sheet password"
              type="password"
              autoComplete="new-password"
              required
              value={sheetPassword}
              onChange={(e) => setSheetPassword(e.target.value)}
            />
            <Button type="submit" variant="primary" disabled={!sheetName || !sheetPassword} className="shrink-0">
              Create sheet
            </Button>
          </form>
        )}
        <DataTable caption="Sheets" columns={sheetColumns} rows={p.sheets_list} rowKey={(s) => s.name} emptyTitle="No sheets" />
      </GlassCard>
    </div>
  );
};

export default ProjectDetailPage;
