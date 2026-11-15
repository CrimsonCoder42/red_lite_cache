package api

// The admin portal's store (S11, migration 000003): admin users, the bootstrap latch, cookie
// sessions and the audit read. Postgres only: with persistence off there is no AdminStore and the
// routes that need it answer 503 "persistence required" (spec criterion 12).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CrimsonCoder42/red_lite_cache/db"
	adminq "github.com/CrimsonCoder42/red_lite_cache/db/sqlc/admin"
	"github.com/CrimsonCoder42/red_lite_cache/internal/adminauth"
)

// ErrLastSuperAdmin refuses demoting or disabling the last active super_admin (409).
var ErrLastSuperAdmin = errors.New("api: last active super_admin")

// AdminUser is one admin_users row without its hash.
type AdminUser struct {
	ID                 uuid.UUID
	Email, Name        string
	Role               adminauth.Role
	MustChangePassword bool
	FailedLogins       int
	LockedUntil        time.Time // zero = not locked
	DisabledAt         time.Time // zero = active
	CreatedBy          uuid.UUID // Nil = the bearer admin key or bootstrap
	CreatedAt          time.Time
	PasswordChangedAt  time.Time
	LastLoginAt        time.Time
}

// LoginUser is what the sign-in check reads for one email.
type LoginUser struct {
	ID           uuid.UUID
	PasswordHash string
	Role         adminauth.Role
	LockedUntil  time.Time // the epoch when not locked
	Disabled     bool
}

// SessionUser is the signed-in user a valid session cookie resolves to.
type SessionUser struct {
	ID                 uuid.UUID
	Email              string
	Role               adminauth.Role
	MustChangePassword bool
}

// AuditFilter selects one page of GET /admin/audit. Cursor fields are set when HasCursor.
type AuditFilter struct {
	Since                          time.Time
	Project                        uuid.UUID // Nil = every project
	Action                         string    // "" = every action
	HasCursor                      bool
	CurAt                          time.Time
	CurNode, CurAction, CurSubject string
	Limit                          int
}

// AdminStore is the admin portal's persistence. Every method is one statement or one transaction.
type AdminStore interface {
	BootstrapDone(ctx context.Context) (bool, error)
	// Bootstrap creates the first super_admin; uuid.Nil when bootstrap already ran (the latch).
	Bootstrap(ctx context.Context, email, name, hash string) (uuid.UUID, error)
	// UserForLogin reads one email's sign-in row (ErrNotFound when unknown).
	UserForLogin(ctx context.Context, email string) (LoginUser, error)
	// LoginRecord records one attempt; returns locked_until (the epoch when not locked).
	LoginRecord(ctx context.Context, id uuid.UUID, ok bool) (time.Time, error)
	SessionOpen(ctx context.Context, tokenHash []byte, user uuid.UUID, ip, userAgent string) (time.Time, error)
	// SessionCheck validates and slides a session (ErrNotFound when it is not valid).
	SessionCheck(ctx context.Context, tokenHash []byte) (SessionUser, error)
	SessionDelete(ctx context.Context, tokenHash []byte) error
	User(ctx context.Context, id uuid.UUID) (AdminUser, error)
	PasswordHash(ctx context.Context, id uuid.UUID) (string, error)
	Users(ctx context.Context, afterCreated time.Time, afterID uuid.UUID, limit int) ([]AdminUser, error)
	// CreateUser stores a user with must_change_password set (ErrConflict: email taken).
	CreateUser(ctx context.Context, email, name string, role adminauth.Role, hash string, createdBy uuid.UUID) (AdminUser, error)
	// SetPassword stores a new hash and ends every session of the user except keep (nil = all),
	// in one transaction.
	SetPassword(ctx context.Context, id uuid.UUID, hash string, mustChange bool, keep []byte) error
	// UpdateUser changes role and/or disabled (nil = unchanged) in one transaction that locks the
	// active super_admins first; disabling ends every session (ErrNotFound, ErrLastSuperAdmin).
	UpdateUser(ctx context.Context, id uuid.UUID, role *adminauth.Role, disabled *bool) (AdminUser, error)
	AuditPage(ctx context.Context, f AuditFilter) ([]adminq.AuditEvent, error)
}

// AdminDB is the AdminStore over db/query/admin.
type AdminDB struct {
	pool *pgxpool.Pool
	q    adminq.Querier
}

// NewAdminDB wraps the process's db.Store (its pool; db.Store carries no admin querier).
func NewAdminDB(store *db.Store) *AdminDB {
	return &AdminDB{pool: store.Pool(), q: adminq.New(store.Pool())}
}

func (d *AdminDB) tx(ctx context.Context, fn func(adminq.Querier) error) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error { return fn(adminq.New(tx)) })
}

func (d *AdminDB) BootstrapDone(ctx context.Context) (bool, error) {
	done, err := d.q.AdminBootstrapDone(ctx)
	if err != nil {
		return false, fmt.Errorf("api: bootstrap done: %w", err)
	}
	return done, nil
}

func (d *AdminDB) Bootstrap(ctx context.Context, email, name, hash string) (uuid.UUID, error) {
	id, err := d.q.AdminBootstrap(ctx, adminq.AdminBootstrapParams{Email: email, Name: name, PasswordHash: hash})
	if err != nil {
		return uuid.Nil, fmt.Errorf("api: bootstrap: %w", err)
	}
	return id, nil
}

func (d *AdminDB) UserForLogin(ctx context.Context, email string) (LoginUser, error) {
	row, err := d.q.AdminUserForLogin(ctx, email)
	if err != nil {
		return LoginUser{}, notFound(err, "user for login")
	}
	return LoginUser{
		ID: row.ID, PasswordHash: row.PasswordHash, Role: adminauth.ParseRole(row.Role),
		LockedUntil: row.LockedUntil, Disabled: row.Disabled,
	}, nil
}

func (d *AdminDB) LoginRecord(ctx context.Context, id uuid.UUID, ok bool) (time.Time, error) {
	t, err := d.q.AdminLoginRecord(ctx, adminq.AdminLoginRecordParams{
		UserID: id, Ok: ok, MaxFailures: adminauth.LockAfter, LockSeconds: int32(adminauth.LockFor / time.Second),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("api: login record: %w", err)
	}
	return t, nil
}

func (d *AdminDB) SessionOpen(ctx context.Context, tokenHash []byte, user uuid.UUID, ip, userAgent string) (time.Time, error) {
	t, err := d.q.AdminSessionOpen(ctx, adminq.AdminSessionOpenParams{
		TokenHash: tokenHash, UserID: user, Ip: ip, UserAgent: userAgent,
		TtlSeconds: int32(adminauth.SessionTTL / time.Second),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("api: session open: %w", err)
	}
	return t, nil
}

func (d *AdminDB) SessionCheck(ctx context.Context, tokenHash []byte) (SessionUser, error) {
	row, err := d.q.AdminSessionCheck(ctx, adminq.AdminSessionCheckParams{
		TokenHash: tokenHash, IdleSeconds: int32(adminauth.SessionIdle / time.Second),
	})
	if err != nil {
		return SessionUser{}, notFound(err, "session check")
	}
	return SessionUser{ID: row.UserID, Email: row.Email, Role: adminauth.ParseRole(row.Role), MustChangePassword: row.MustChangePassword}, nil
}

func (d *AdminDB) SessionDelete(ctx context.Context, tokenHash []byte) error {
	if err := d.q.AdminSessionDelete(ctx, tokenHash); err != nil {
		return fmt.Errorf("api: session delete: %w", err)
	}
	return nil
}

func (d *AdminDB) User(ctx context.Context, id uuid.UUID) (AdminUser, error) {
	row, err := d.q.AdminUserGet(ctx, id)
	if err != nil {
		return AdminUser{}, notFound(err, "get user")
	}
	return adminUser(row), nil
}

func (d *AdminDB) PasswordHash(ctx context.Context, id uuid.UUID) (string, error) {
	h, err := d.q.AdminUserPasswordHash(ctx, id)
	if err != nil {
		return "", notFound(err, "password hash")
	}
	return h, nil
}

func (d *AdminDB) Users(ctx context.Context, afterCreated time.Time, afterID uuid.UUID, limit int) ([]AdminUser, error) {
	rows, err := d.q.AdminUsersPage(ctx, adminq.AdminUsersPageParams{
		AfterCreatedAt: afterCreated, AfterID: afterID, PageLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("api: list users: %w", err)
	}
	out := make([]AdminUser, len(rows))
	for i, r := range rows {
		out[i] = adminUser(adminq.AdminUserGetRow(r))
	}
	return out, nil
}

func (d *AdminDB) CreateUser(ctx context.Context, email, name string, role adminauth.Role, hash string, createdBy uuid.UUID) (AdminUser, error) {
	row, err := d.q.AdminUserCreate(ctx, adminq.AdminUserCreateParams{
		Email: email, Name: name, Role: role.String(), PasswordHash: hash,
		CreatedBy: pgtype.UUID{Bytes: createdBy, Valid: createdBy != uuid.Nil},
	})
	if err != nil {
		if isUniqueViolation(err) {
			return AdminUser{}, ErrConflict
		}
		return AdminUser{}, fmt.Errorf("api: create user: %w", err)
	}
	return adminUser(adminq.AdminUserGetRow(row)), nil
}

func (d *AdminDB) SetPassword(ctx context.Context, id uuid.UUID, hash string, mustChange bool, keep []byte) error {
	return d.tx(ctx, func(q adminq.Querier) error {
		n, err := q.AdminUserSetPassword(ctx, adminq.AdminUserSetPasswordParams{PasswordHash: hash, MustChange: mustChange, ID: id})
		if err != nil {
			return fmt.Errorf("api: set password: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		if keep == nil {
			keep = []byte{}
		}
		if _, err := q.AdminSessionsDeleteForUser(ctx, adminq.AdminSessionsDeleteForUserParams{UserID: id, Keep: keep}); err != nil {
			return fmt.Errorf("api: end sessions: %w", err)
		}
		return nil
	})
}

func (d *AdminDB) UpdateUser(ctx context.Context, id uuid.UUID, role *adminauth.Role, disabled *bool) (AdminUser, error) {
	var out AdminUser
	err := d.tx(ctx, func(q adminq.Querier) error {
		// Lock every active super_admin row first (AdminActiveSuperAdminsLock), then decide.
		supers, err := q.AdminActiveSuperAdminsLock(ctx)
		if err != nil {
			return fmt.Errorf("api: lock super admins: %w", err)
		}
		cur, err := q.AdminUserGet(ctx, id)
		if err != nil {
			return notFound(err, "get user")
		}
		losesSuper := cur.Role == "super_admin" && !cur.DisabledAt.Valid &&
			((role != nil && *role != adminauth.RoleSuperAdmin) || (disabled != nil && *disabled))
		if losesSuper && len(supers) <= 1 {
			return ErrLastSuperAdmin
		}
		p := adminq.AdminUserUpdateParams{ID: id}
		if role != nil {
			p.Role = pgtype.Text{String: role.String(), Valid: true}
		}
		if disabled != nil {
			p.Disabled = pgtype.Bool{Bool: *disabled, Valid: true}
		}
		row, err := q.AdminUserUpdate(ctx, p)
		if err != nil {
			return notFound(err, "update user")
		}
		if disabled != nil && *disabled {
			if _, err := q.AdminSessionsDeleteForUser(ctx, adminq.AdminSessionsDeleteForUserParams{UserID: id, Keep: []byte{}}); err != nil {
				return fmt.Errorf("api: end sessions: %w", err)
			}
		}
		out = adminUser(adminq.AdminUserGetRow(row))
		return nil
	})
	return out, err
}

func (d *AdminDB) AuditPage(ctx context.Context, f AuditFilter) ([]adminq.AuditEvent, error) {
	rows, err := d.q.AuditPage(ctx, adminq.AuditPageParams{
		Since:      f.Since,
		ProjectID:  pgtype.UUID{Bytes: f.Project, Valid: f.Project != uuid.Nil},
		Action:     pgtype.Text{String: f.Action, Valid: f.Action != ""},
		HasCursor:  f.HasCursor,
		CurAt:      f.CurAt,
		CurNode:    f.CurNode,
		CurAction:  f.CurAction,
		CurSubject: f.CurSubject,
		PageLimit:  int32(f.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("api: audit page: %w", err)
	}
	return rows, nil
}

func adminUser(r adminq.AdminUserGetRow) AdminUser {
	u := AdminUser{
		ID: r.ID, Email: r.Email, Name: r.Name, Role: adminauth.ParseRole(r.Role),
		MustChangePassword: r.MustChangePassword, FailedLogins: int(r.FailedLogins),
		LockedUntil: tsTime(r.LockedUntil), DisabledAt: tsTime(r.DisabledAt),
		CreatedAt: r.CreatedAt, PasswordChangedAt: r.PasswordChangedAt, LastLoginAt: tsTime(r.LastLoginAt),
	}
	if r.CreatedBy.Valid {
		u.CreatedBy = r.CreatedBy.Bytes
	}
	return u
}
