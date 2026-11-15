/**
 * api/client.ts -- the one way the portal talks to the admin port.
 *
 * fetch with credentials 'same-origin': the session is the HttpOnly cookie the server sets on
 * POST /admin/auth/login (internal/api/admin_auth.go setSessionCookie); JavaScript never sees or
 * stores a token, a key or a password. Bodies are JSON; an error body is {"error": "..."}
 * (protocol.APIError). The reference admin's apiClient.ts (Bearer token from localStorage, manual
 * refresh) is deliberately not copied: there is no token to refresh.
 *
 * Status handling (request): 401 -> the auth context sends the user to /login; 403 -> a toast;
 * 410 / 429 -> the server's own message. Every caller gets an ApiError it can show.
 */

export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

type UnauthorizedHandler = () => void;
let onUnauthorized: UnauthorizedHandler | null = null;

/** The auth context registers what a 401 does (clear the user, go to /login). */
export function setUnauthorizedHandler(fn: UnauthorizedHandler | null): void {
  onUnauthorized = fn;
}

export async function api<T = void>(
  method: 'GET' | 'POST' | 'PATCH' | 'DELETE',
  path: string,
  body?: unknown,
): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  let data: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const msg =
      data && typeof data === 'object' && 'error' in data && typeof (data as { error: unknown }).error === 'string'
        ? (data as { error: string }).error
        : `request failed (${res.status})`;
    // A 401 on the sign-in call itself is a wrong password, not an expired session.
    if (res.status === 401 && path !== '/admin/auth/login' && onUnauthorized) onUnauthorized();
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

/** Path segment escape for ids and sheet names. */
export const seg = (s: string): string => encodeURIComponent(s);
