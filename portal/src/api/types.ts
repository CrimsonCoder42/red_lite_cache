/**
 * api/types.ts -- the JSON the admin port answers, field for field.
 *
 * Sources (the only API this portal calls): protocol/admin.go (projects, keys, sheets, sessions,
 * Page, APIError) and internal/api/admin_types.go (auth, users, stats, audit). Times are RFC 3339 UTC
 * strings; "" or an absent field means none.
 */

export type Role = 'viewer' | 'admin' | 'super_admin';

export interface Page<T> {
  items: T[];
  next_cursor?: string;
}

export interface UserInfo {
  user_id: string;
  email: string;
  name: string;
  role: Role;
  must_change_password: boolean;
  locked_until?: string;
  disabled_at?: string;
  created_by?: string;
  created_at: string;
  password_changed_at: string;
  last_login_at?: string;
}

export interface MeResponse {
  actor: string;
  role: Role;
  must_change_password: boolean;
  expires_at?: string;
  user?: UserInfo;
}

export interface ProjectSummary {
  project_id: string;
  name: string;
  created_at: string;
  suspended_at?: string;
  keys: number;
  sheets: number;
}

export interface KeyInfo {
  key_id: string;
  key_prefix: string;
  label?: string;
  created_at: string;
  revoked_at?: string;
  last_used_at?: string;
}

export interface SheetInfo {
  name: string;
  keys: number;
  collaborators: number;
}

export interface ProjectDetail extends ProjectSummary {
  api_keys: KeyInfo[];
  sheets_list: SheetInfo[];
}

export interface CreateProjectResponse {
  project_id: string;
  name: string;
  api_key: string;
  key_prefix: string;
}

export interface IssuedKey {
  key_id: string;
  project_id: string;
  api_key: string;
  key_prefix: string;
  label?: string;
}

export interface CreateSheetResponse {
  sheet: string;
  project_id: string;
}
