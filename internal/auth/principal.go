package auth

import (
	"context"
	"fmt"

	"example.com/homecloud/internal/errs"
)

// AnyProject in a binding means "every project" (and, for requests without a project, "global").
const AnyProject = "*"

type PrincipalType string

const (
	User    PrincipalType = "user"
	Service PrincipalType = "service"
)

type Binding struct {
	Project string `json:"p"`
	Role    Role   `json:"r"`
}

type Principal struct {
	Subject  string // e.g. "user:robert", "svc:agentd"
	Type     PrincipalType
	Bindings []Binding
	TokenID  string // jti, for audit and revocation
}

func (p *Principal) matches(b Binding, project string) bool {
	return b.Project == AnyProject || (project != "" && b.Project == project)
}

// Can reports whether the principal holds perm on project. An empty project means a global
// (non-project) resource and is only satisfied by a "*" binding.
func (p *Principal) Can(perm Permission, project string) bool {
	for _, b := range p.Bindings {
		if p.matches(b, project) && b.Role.Grants(perm) {
			return true
		}
	}
	return false
}

// Visible reports whether the principal has any role at all on project. Used to answer
// "not found" instead of "forbidden" so resource IDs of other tenants cannot be probed.
func (p *Principal) Visible(project string) bool {
	for _, b := range p.Bindings {
		if p.matches(b, project) {
			return true
		}
	}
	return false
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok
}

// Require checks perm on project for the caller in ctx. Use it for requests that name a project
// directly (create/list). Returns ErrUnauthenticated / ErrPermissionDenied.
func Require(ctx context.Context, perm Permission, project string) error {
	p, ok := FromContext(ctx)
	if !ok {
		return errs.ErrUnauthenticated
	}
	if !p.Can(perm, project) {
		return fmt.Errorf("%w: %s lacks %s on %q", errs.ErrPermissionDenied, p.Subject, perm, project)
	}
	return nil
}

// RequireResource is Require for a resource addressed by ID: callers with no access to the
// resource's project get NotFound (indistinguishable from a missing ID); callers who can see the
// project but lack the permission get PermissionDenied.
func RequireResource(ctx context.Context, perm Permission, project, kind, id string) error {
	p, ok := FromContext(ctx)
	if !ok {
		return errs.ErrUnauthenticated
	}
	if !p.Visible(project) {
		return errs.NotFound("%s %s", kind, id)
	}
	if !p.Can(perm, project) {
		return fmt.Errorf("%w: %s lacks %s on %q", errs.ErrPermissionDenied, p.Subject, perm, project)
	}
	return nil
}

// RequireAnyProject checks that the caller holds perm in at least one project (or globally). It guards
// resources shared across projects, such as shared images, which any tenant may read.
func RequireAnyProject(ctx context.Context, perm Permission) error {
	p, ok := FromContext(ctx)
	if !ok {
		return errs.ErrUnauthenticated
	}
	for _, b := range p.Bindings {
		if b.Role.Grants(perm) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s lacks %s", errs.ErrPermissionDenied, p.Subject, perm)
}
