package auth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/gen/homecloud/vmd/v1/vmdv1connect"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/errs"
)

func newIssuer(t *testing.T, kid string) (*auth.Issuer, auth.TrustedKey) {
	t.Helper()
	pub, priv, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &auth.Issuer{KeyID: kid, Key: priv}, auth.TrustedKey{Public: pub}
}

func editorOn(project string) auth.Principal {
	return auth.Principal{Subject: "user:robert", Type: auth.User, Bindings: []auth.Binding{{Project: project, Role: auth.RoleEditor}}}
}

func TestRolePermissionMatrix(t *testing.T) {
	cases := []struct {
		role auth.Role
		perm auth.Permission
		want bool
	}{
		{auth.RoleViewer, auth.VMGet, true},
		{auth.RoleViewer, auth.VMCreate, false},
		{auth.RoleViewer, auth.ExecRun, false},
		{auth.RoleEditor, auth.VMCreate, true},
		{auth.RoleEditor, auth.ExecRun, true},
		{auth.RoleEditor, auth.VMHold, false},
		{auth.RoleEditor, auth.ImageRegister, false},
		{auth.RoleAdmin, auth.ImageRegister, true},
		{auth.RoleService, auth.VMHold, true},
		{auth.RoleService, auth.ExecRun, true},
		{auth.RoleService, auth.VMDelete, false},
		{auth.RoleService, auth.VMCreate, false},
		{auth.RoleNode, auth.NodeManage, true},
		{auth.RoleAdmin, auth.NodeManage, false},
		{auth.Role("nope"), auth.VMGet, false},
	}
	for _, c := range cases {
		if got := c.role.Grants(c.perm); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestPrincipalProjectScoping(t *testing.T) {
	p := editorOn("p1")
	if !p.Can(auth.VMCreate, "p1") || p.Can(auth.VMCreate, "p2") || p.Can(auth.VMCreate, "") {
		t.Fatal("project binding must not leak to other projects or the global scope")
	}
	global := auth.Principal{Bindings: []auth.Binding{{Project: auth.AnyProject, Role: auth.RoleAdmin}}}
	if !global.Can(auth.ImageRegister, "") || !global.Can(auth.VMCreate, "anything") {
		t.Fatal("'*' binding applies everywhere including global")
	}
	if !p.Visible("p1") || p.Visible("p2") {
		t.Fatal("visibility")
	}
}

func TestRequireAndRequireResource(t *testing.T) {
	if err := auth.Require(context.Background(), auth.VMGet, "p1"); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("no principal: %v", err)
	}
	viewer := auth.Principal{Subject: "user:v", Bindings: []auth.Binding{{Project: "p1", Role: auth.RoleViewer}}}
	ctx := auth.WithPrincipal(context.Background(), &viewer)
	if err := auth.Require(ctx, auth.VMGet, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := auth.Require(ctx, auth.VMCreate, "p1"); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("want denied, got %v", err)
	}
	// resource in a project the caller cannot see looks exactly like a missing one
	if err := auth.RequireResource(ctx, auth.VMGet, "other", "vm", "vm_1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := auth.RequireResource(ctx, auth.VMDelete, "p1", "vm", "vm_1"); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("want denied, got %v", err)
	}
}

func TestTokenRoundTripAndRejections(t *testing.T) {
	iss, trusted := newIssuer(t, "k1")
	v := &auth.Verifier{Keys: map[string]auth.TrustedKey{"k1": trusted}, MaxTTL: time.Hour}

	tok, err := iss.Mint(editorOn("p1"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "user:robert" || !p.Can(auth.VMCreate, "p1") || p.TokenID == "" {
		t.Fatalf("bad principal %+v", p)
	}

	t.Run("expired", func(t *testing.T) {
		old := &auth.Issuer{KeyID: "k1", Key: iss.Key, Now: func() time.Time { return time.Now().Add(-2 * time.Hour) }}
		tok, _ := old.Mint(editorOn("p1"), time.Minute)
		if _, err := v.Verify(tok); !errors.Is(err, errs.ErrUnauthenticated) {
			t.Fatalf("want unauthenticated, got %v", err)
		}
	})
	t.Run("tampered payload", func(t *testing.T) {
		parts := strings.Split(tok, ".")
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		forged := strings.Replace(string(raw), `"editor"`, `"admin"`, 1)
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte(forged))
		if _, err := v.Verify(strings.Join(parts, ".")); err == nil {
			t.Fatal("tampered token accepted")
		}
	})
	t.Run("unknown key id", func(t *testing.T) {
		other, _ := newIssuer(t, "evil")
		tok, _ := other.Mint(editorOn("p1"), time.Minute)
		if _, err := v.Verify(tok); err == nil {
			t.Fatal("token from untrusted key accepted")
		}
	})
	t.Run("right kid but wrong key", func(t *testing.T) {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		forger := &auth.Issuer{KeyID: "k1", Key: priv}
		tok, _ := forger.Mint(editorOn("p1"), time.Minute)
		if _, err := v.Verify(tok); err == nil {
			t.Fatal("forged signature accepted")
		}
	})
	t.Run("alg none", func(t *testing.T) {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1","typ":"JWT"}`))
		body := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"k1","sub":"x","exp":9999999999,"b":[{"p":"*","r":"admin"}]}`))
		if _, err := v.Verify(hdr + "." + body + "."); err == nil {
			t.Fatal("unsigned token accepted")
		}
	})
	t.Run("lifetime above max", func(t *testing.T) {
		long, _ := iss.Mint(editorOn("p1"), 48*time.Hour)
		if _, err := v.Verify(long); err == nil {
			t.Fatal("over-long token accepted")
		}
	})
	t.Run("revoked", func(t *testing.T) {
		rv := &auth.Verifier{Keys: v.Keys, Revocations: revoked{p.TokenID: true}}
		if _, err := rv.Verify(tok); err == nil {
			t.Fatal("revoked token accepted")
		}
	})
	t.Run("unknown role refused at mint", func(t *testing.T) {
		bad := auth.Principal{Subject: "x", Bindings: []auth.Binding{{Project: "p", Role: "root"}}}
		if _, err := iss.Mint(bad, time.Minute); err == nil {
			t.Fatal("minted a token with an unknown role")
		}
	})
}

type revoked map[string]bool

func (r revoked) Revoked(jti string) bool { return r[jti] }

func TestKeyRestrictsRoles(t *testing.T) {
	iss, trusted := newIssuer(t, "agentd")
	trusted.AllowedRoles = []auth.Role{auth.RoleService}
	v := &auth.Verifier{Keys: map[string]auth.TrustedKey{"agentd": trusted}}

	svc := auth.Principal{Subject: "svc:agentd", Type: auth.Service, Bindings: []auth.Binding{{Project: auth.AnyProject, Role: auth.RoleService}}}
	tok, _ := iss.Mint(svc, time.Minute)
	if _, err := v.Verify(tok); err != nil {
		t.Fatal(err)
	}
	evil := auth.Principal{Subject: "svc:agentd", Type: auth.Service, Bindings: []auth.Binding{{Project: auth.AnyProject, Role: auth.RoleAdmin}}}
	tok, _ = iss.Mint(evil, time.Minute)
	if _, err := v.Verify(tok); err == nil {
		t.Fatal("a compromised service key must not be able to mint admin tokens")
	}
}

func TestKeyFilesRoundTrip(t *testing.T) {
	pub, priv, _ := auth.GenerateKey()
	dir := t.TempDir()
	if err := auth.SavePrivateKey(filepath.Join(dir, "k.pem"), priv); err != nil {
		t.Fatal(err)
	}
	if err := auth.SavePublicKey(filepath.Join(dir, "k.pub"), pub); err != nil {
		t.Fatal(err)
	}
	p2, err := auth.LoadPrivateKey(filepath.Join(dir, "k.pem"))
	if err != nil || !p2.Equal(priv) {
		t.Fatalf("private key round trip: %v", err)
	}
	pub2, err := auth.LoadPublicKey(filepath.Join(dir, "k.pub"))
	if err != nil || !pub2.Equal(pub) {
		t.Fatalf("public key round trip: %v", err)
	}
	if _, err := auth.LoadPrivateKey(filepath.Join(dir, "k.pub")); err == nil {
		t.Fatal("public key file loaded as private key")
	}
}

// who echoes the authenticated subject back, so the interceptor can be tested end to end over HTTP.
type who struct {
	vmdv1connect.UnimplementedVMServiceHandler
}

func (who) GetVM(ctx context.Context, _ *connect.Request[vmdv1.GetVMRequest]) (*connect.Response[vmdv1.VM], error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, errs.ToConnect(errs.ErrUnauthenticated)
	}
	return connect.NewResponse(&vmdv1.VM{Name: p.Subject}), nil
}

func TestInterceptorOverHTTP(t *testing.T) {
	iss, trusted := newIssuer(t, "k1")
	v := &auth.Verifier{Keys: map[string]auth.TrustedKey{"k1": trusted}}
	mux := http.NewServeMux()
	mux.Handle(vmdv1connect.NewVMServiceHandler(who{}, connect.WithInterceptors(&auth.ServerInterceptor{Verifier: v})))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	call := func(src auth.TokenSource) error {
		opts := []connect.ClientOption{}
		if src != nil {
			opts = append(opts, connect.WithInterceptors(auth.ClientInterceptor(src)))
		}
		c := vmdv1connect.NewVMServiceClient(http.DefaultClient, srv.URL, opts...)
		res, err := c.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: "x"}))
		if err == nil && res.Msg.Name != "user:robert" {
			t.Errorf("subject = %q", res.Msg.Name)
		}
		return err
	}

	if err := call(nil); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token: %v", err)
	}
	if err := call(auth.StaticToken("garbage")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("garbage token: %v", err)
	}
	if err := call(auth.ServiceTokens(iss, editorOn("p1"), time.Minute)); err != nil {
		t.Fatalf("valid token: %v", err)
	}
}

func TestServiceTokensAreCached(t *testing.T) {
	iss, _ := newIssuer(t, "k1")
	now := time.Now()
	iss.Now = func() time.Time { return now }
	src := auth.ServiceTokens(iss, editorOn("p1"), time.Minute)
	a, _ := src(context.Background())
	b, _ := src(context.Background())
	if a != b {
		t.Fatal("token should be reused while fresh")
	}
	now = now.Add(55 * time.Second) // inside the last fifth of its life: renew
	c, _ := src(context.Background())
	if c == a {
		t.Fatal("token should be renewed before it expires")
	}
}

func TestOperatorAndMetricsPermissions(t *testing.T) {
	if !auth.RoleOperator.Grants(auth.CapacityGet) || auth.RoleOperator.Grants(auth.VMGet) {
		t.Fatal("operators see capacity only, nothing inside projects")
	}
	for _, r := range []auth.Role{auth.RoleViewer, auth.RoleEditor, auth.RoleAdmin, auth.RoleService} {
		if !r.Grants(auth.VMMetrics) {
			t.Errorf("%s should read VM metrics", r)
		}
		if r.Grants(auth.CapacityGet) {
			t.Errorf("%s must not read cluster capacity", r)
		}
	}
	// capacity is global: a project-scoped operator binding must not satisfy it
	p := auth.Principal{Bindings: []auth.Binding{{Project: "p1", Role: auth.RoleOperator}}}
	if p.Can(auth.CapacityGet, "") {
		t.Fatal("capacity.get needs a '*' binding")
	}
	g := auth.Principal{Bindings: []auth.Binding{{Project: auth.AnyProject, Role: auth.RoleOperator}}}
	if !g.Can(auth.CapacityGet, "") {
		t.Fatal("'*' operator should see capacity")
	}
}

func TestRequireAnyProject(t *testing.T) {
	if err := auth.RequireAnyProject(context.Background(), auth.ImageGet); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("no principal: %v", err)
	}
	viewer := auth.Principal{Subject: "u", Bindings: []auth.Binding{{Project: "p1", Role: auth.RoleViewer}}}
	ctx := auth.WithPrincipal(context.Background(), &viewer)
	if err := auth.RequireAnyProject(ctx, auth.ImageGet); err != nil {
		t.Fatalf("viewer on p1 may read shared images: %v", err)
	}
	if err := auth.RequireAnyProject(ctx, auth.VMCreate); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("viewer may not create: %v", err)
	}
}

// Base64url decoding must be strict: flipping the unused trailing bits of the signature's last
// character may not produce a second valid spelling of the same token.
func TestTokenHasOneCanonicalForm(t *testing.T) {
	iss, trusted := newIssuer(t, "k1")
	v := &auth.Verifier{Keys: map[string]auth.TrustedKey{"k1": trusted}}
	tok, _ := iss.Mint(editorOn("p1"), time.Minute)
	if _, err := v.Verify(tok); err != nil {
		t.Fatal(err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	accepted := 0
	for _, c := range alphabet {
		variant := tok[:len(tok)-1] + string(c)
		if variant != tok {
			if _, err := v.Verify(variant); err == nil {
				accepted++
			}
		}
	}
	if accepted != 0 {
		t.Fatalf("%d non-canonical spellings of the token verified", accepted)
	}
}

func TestRolePermissionsAreListed(t *testing.T) {
	if len(auth.Roles()) < 6 {
		t.Fatalf("roles: %v", auth.Roles())
	}
	for _, r := range auth.Roles() {
		perms := r.Permissions()
		if len(perms) == 0 {
			t.Errorf("%s has no permissions", r)
		}
		for _, p := range perms {
			if !r.Grants(p) {
				t.Errorf("%s lists %s but does not grant it", r, p)
			}
		}
	}
}
