/**
 * pages/ProjectsPage.tsx -- the project list and "create project".
 *
 *   list    GET  /admin/projects  (viewer; internal/api/tenancy.go listProjects, keyset paged)
 *   create  POST /admin/projects  (admin; internal/api/api.go createProject) -> the project's first
 *           API key, shown once (SecretDialog) with a ready .env line.
 */

import React, { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '@/api/client';
import type { CreateProjectResponse, ProjectSummary } from '@/api/types';
import { usePagedList } from '@/api/usePagedList';
import { atLeast, useAuth } from '@/auth/AuthContext';
import { Badge, Button, DataTable, DateCell, GlassCard, Input, Mono, Primary, useModal, type Column } from '@/features/ui';
import { showSecret } from '@/features/ui/SecretDialog';
import { useToast } from '@/features/ui/Toast';

/** The .env variable the copied line uses (PLAN.md §6); a consumer passes its value as the reader's
 * Config.APIKey (reader/reader.go:70). The reader itself reads no environment. */
export const ENV_KEY = 'REDLITE_API_KEY';

export const ProjectsPage: React.FC = () => {
  const { me } = useAuth();
  const canWrite = atLeast(me?.role, 'admin');
  const navigate = useNavigate();
  const modal = useModal();
  const toast = useToast();
  const list = usePagedList<ProjectSummary>('/admin/projects');
  const [name, setName] = useState('');
  const [label, setLabel] = useState('');
  const [busy, setBusy] = useState(false);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      const p = await api<CreateProjectResponse>('POST', '/admin/projects', { name, label: label || undefined });
      setName('');
      setLabel('');
      list.reload();
      showSecret(modal, {
        title: `Project ${p.name} created`,
        label: 'API key',
        secret: p.api_key,
        envLine: `${ENV_KEY}=${p.api_key}`,
        note: (
          <>
            Key prefix <Mono>{p.key_prefix}</Mono>. Project id <Mono>{p.project_id}</Mono>.
          </>
        ),
      });
    } catch (err) {
      toast.fail(err);
    } finally {
      setBusy(false);
    }
  };

  const columns: Column<ProjectSummary>[] = [
    { key: 'name', header: 'Project', render: (p) => <Primary>{p.name}</Primary> },
    {
      key: 'status',
      header: 'Status',
      render: (p) => (p.suspended_at ? <Badge tone="warning">Suspended</Badge> : <Badge tone="success">Active</Badge>),
    },
    { key: 'keys', header: 'Live keys', align: 'right', render: (p) => p.keys },
    { key: 'sheets', header: 'Sheets', align: 'right', render: (p) => p.sheets },
    { key: 'created', header: 'Created', render: (p) => <DateCell iso={p.created_at} /> },
  ];

  return (
    <div className="space-y-6">
      {canWrite && (
        <GlassCard title="Create project" subtitle="Its first API key is shown once, right after.">
          <form onSubmit={create} className="flex flex-col gap-4 md:flex-row md:items-end">
            <Input label="Name" required value={name} onChange={(e) => setName(e.target.value)} />
            <Input label="First key label (optional)" value={label} maxLength={200} onChange={(e) => setLabel(e.target.value)} />
            <Button type="submit" variant="primary" disabled={busy || !name} className="shrink-0">
              {busy ? 'Creating...' : 'Create project'}
            </Button>
          </form>
        </GlassCard>
      )}

      <GlassCard title="Projects" flush>
        <DataTable
          caption="Projects"
          columns={columns}
          rows={list.rows}
          rowKey={(p) => p.project_id}
          loading={list.loading}
          error={list.error}
          emptyTitle="No projects yet"
          pagination={list.pagination}
          onRowClick={(p) => navigate(`/projects/${p.project_id}`)}
        />
      </GlassCard>
    </div>
  );
};

export default ProjectsPage;
