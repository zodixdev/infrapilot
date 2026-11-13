package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zodixdev/infrapilot/pkg/postgres"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/store/db"
)

// Membership changes lock rows in one global order — every owner row first (LockOwners, ordered by user id),
// then the target row (LockMember) — so concurrent changes cannot deadlock, and two concurrent demotions of the
// last two owners serialize: the second re-reads the owners after the first commits and sees only one.

// AddMember adds a user who has signed in at least once, by e-mail (member.manage).
func (s *Service) AddMember(ctx context.Context, a Actor, org uuid.UUID, email string, role domain.Role) (domain.Member, error) {
	var v domain.Validator
	email = domain.NormalizeEmail(&v, "email", email)
	v.Check(role.Valid(), "role", "must be one of owner, admin, operator, viewer")
	if err := v.Err(); err != nil {
		return domain.Member{}, err
	}
	var out domain.Member
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := actorRoleFor(ctx, q, a, org); err != nil {
			return err
		}
		owners, err := q.LockOwners(ctx, org)
		if err != nil {
			return fmt.Errorf("add member: %w", err)
		}
		// The actor's role may have changed while waiting for the owner locks; decide on the current one.
		r, err := q.GetRole(ctx, db.GetRoleParams{OrganizationID: org, UserID: a.ID})
		if err != nil {
			return notFound(err, "organization %s", org)
		}
		actorRole := domain.Role(r)
		if denied := domain.CheckMemberChange(domain.MemberChange{ActorRole: actorRole, New: role, Owners: len(owners)}); denied != nil {
			return denied
		}
		users, err := q.FindUsersByEmail(ctx, email)
		if err != nil {
			return fmt.Errorf("add member: %w", err)
		}
		switch len(users) {
		case 0:
			return domain.NotFoundf("no user with this verified e-mail has signed in yet")
		case 1:
		default:
			return domain.Preconditionf("several accounts use this e-mail address")
		}
		u := users[0]
		if err = q.InsertMembership(ctx, db.InsertMembershipParams{OrganizationID: org, UserID: u.ID, Role: string(role), CreatedBy: &a.ID}); err != nil {
			return conflictOn(err, map[string]string{"memberships_pkey": "the user is already a member"})
		}
		row, err := q.GetMember(ctx, db.GetMemberParams{OrganizationID: org, UserID: u.ID})
		if err != nil {
			return fmt.Errorf("add member: %w", err)
		}
		out = toMember(&row)
		return audit(ctx, q, org, a, "member.added", "member", u.ID.String(), map[string]string{"role": string(role)})
	})
	return out, err
}

// GetMember returns one membership (member.read).
func (s *Service) GetMember(ctx context.Context, a Actor, org, user uuid.UUID) (domain.Member, error) {
	var out domain.Member
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermMemberRead, uuid.Nil); err != nil {
			return err
		}
		row, err := q.GetMember(ctx, db.GetMemberParams{OrganizationID: org, UserID: user})
		out = toMember(&row)
		return notFound(err, "member %s", user)
	}, postgres.ReadOnly())
	return out, err
}

// ListMembers lists an organization's members (member.read).
func (s *Service) ListMembers(ctx context.Context, a Actor, org uuid.UUID, size int32, token string) ([]domain.Member, string, error) {
	var v domain.Validator
	page := domain.ParsePage(&v, size, token)
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	var rows []db.ListMembersRow
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := authorize(ctx, q, a, org, domain.PermMemberRead, uuid.Nil); err != nil {
			return err
		}
		var err error
		rows, err = q.ListMembers(ctx, db.ListMembersParams{OrganizationID: org, After: page.Cursor, Lim: page.Limit()})
		return err
	}, postgres.ReadOnly())
	if err != nil {
		return nil, "", err
	}
	rows, more := domain.Trim(page, rows)
	out := make([]domain.Member, len(rows))
	var last uuid.UUID
	for i := range rows {
		r := &rows[i]
		out[i] = toMember((*db.GetMemberRow)(r))
		last = r.UserID
	}
	return out, domain.NextToken(more, last), nil
}

// UpdateMemberRole changes a member's role, enforcing the owner invariants of ADR-0022.
func (s *Service) UpdateMemberRole(ctx context.Context, a Actor, org, user uuid.UUID, role domain.Role) (domain.Member, error) {
	var v domain.Validator
	v.Check(role.Valid(), "role", "must be one of owner, admin, operator, viewer")
	if err := v.Err(); err != nil {
		return domain.Member{}, err
	}
	var out domain.Member
	err := s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		current, owners, actorRole, err := lockForChange(ctx, q, a, org, user)
		if err != nil {
			return err
		}
		if denied := domain.CheckMemberChange(domain.MemberChange{ActorRole: actorRole, ActorIsSelf: a.ID == user,
			Current: current, New: role, Owners: owners}); denied != nil {
			return denied
		}
		if current != role {
			if err = q.UpdateMemberRole(ctx, db.UpdateMemberRoleParams{OrganizationID: org, UserID: user, Role: string(role)}); err != nil {
				return fmt.Errorf("update member: %w", err)
			}
			if auditErr := audit(ctx, q, org, a, "member.role_changed", "member", user.String(),
				map[string]string{"old_role": string(current), "new_role": string(role)}); auditErr != nil {
				return auditErr
			}
		}
		row, err := q.GetMember(ctx, db.GetMemberParams{OrganizationID: org, UserID: user})
		out = toMember(&row)
		return err
	})
	return out, err
}

// RemoveMember removes a membership; members may remove themselves (ADR-0022).
func (s *Service) RemoveMember(ctx context.Context, a Actor, org, user uuid.UUID) error {
	return s.pool.WithTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		current, owners, actorRole, err := lockForChange(ctx, q, a, org, user)
		if err != nil {
			return err
		}
		if err := domain.CheckMemberChange(domain.MemberChange{ActorRole: actorRole, ActorIsSelf: a.ID == user,
			Current: current, Owners: owners}); err != nil {
			return err
		}
		if _, err := q.DeleteMember(ctx, db.DeleteMemberParams{OrganizationID: org, UserID: user}); err != nil {
			return fmt.Errorf("remove member: %w", err)
		}
		return audit(ctx, q, org, a, "member.removed", "member", user.String(), map[string]string{"role": string(current)})
	})
}

// actorRoleFor returns the acting user's role; API keys never manage members (ADR-0022).
func actorRoleFor(ctx context.Context, q *db.Queries, a Actor, org uuid.UUID) (domain.Role, error) {
	if a.Kind != domain.ActorUser {
		// Unknown orgs stay NOT_FOUND for keys of other tenants; keys of this org are denied.
		if _, err := authorize(ctx, q, a, org, domain.PermOrgRead, uuid.Nil); err != nil {
			return "", err
		}
		return "", domain.Deniedf("api keys cannot manage members")
	}
	g, err := authorize(ctx, q, a, org, domain.PermMemberRead, uuid.Nil)
	return g.role, err
}

// lockForChange takes the membership locks in the documented order and returns the target's role, the owner
// count and the actor's role.
func lockForChange(ctx context.Context, q *db.Queries, a Actor, org, user uuid.UUID) (domain.Role, int, domain.Role, error) {
	actorRole, err := actorRoleFor(ctx, q, a, org)
	if err != nil {
		return "", 0, "", err
	}
	owners, err := q.LockOwners(ctx, org)
	if err != nil {
		return "", 0, "", fmt.Errorf("lock owners: %w", err)
	}
	current, err := q.LockMember(ctx, db.LockMemberParams{OrganizationID: org, UserID: user})
	if err != nil {
		return "", 0, "", notFound(err, "member %s", user)
	}
	// The actor's own role may have changed while waiting for the locks; re-read it under them.
	if a.Kind == domain.ActorUser {
		r, err := q.GetRole(ctx, db.GetRoleParams{OrganizationID: org, UserID: a.ID})
		if err != nil {
			return "", 0, "", notFound(err, "organization %s", org)
		}
		actorRole = domain.Role(r)
	}
	return domain.Role(current), len(owners), actorRole, nil
}

func toMember(r *db.GetMemberRow) domain.Member {
	return domain.Member{UserID: r.UserID, OrganizationID: r.OrganizationID, Email: r.Email, DisplayName: r.DisplayName,
		Role: domain.Role(r.Role), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
