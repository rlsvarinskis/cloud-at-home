package authz_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

func newIssuer(t *testing.T, kid string) (*authz.Issuer, authz.TrustedKey) {
	t.Helper()
	pub, priv, err := authz.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &authz.Issuer{KeyID: kid, Key: priv}, authz.TrustedKey{Public: pub}
}

func editorOn(project string) authz.Principal {
	return authz.Principal{Subject: "user:robert", Type: authz.User, Bindings: []authz.Binding{{Project: project, Role: authz.RoleEditor}}}
}

func TestRolePermissionMatrix(t *testing.T) {
	cases := []struct {
		role authz.Role
		perm authz.Permission
		want bool
	}{
		{authz.RoleViewer, authz.VMGet, true},
		{authz.RoleViewer, authz.VMCreate, false},
		{authz.RoleViewer, authz.ExecRun, false},
		{authz.RoleEditor, authz.VMCreate, true},
		{authz.RoleEditor, authz.ExecRun, true},
		{authz.RoleEditor, authz.VMHold, false},
		{authz.RoleEditor, authz.ImageRegister, false},
		{authz.RoleAdmin, authz.ImageRegister, true},
		{authz.RoleService, authz.VMHold, true},
		{authz.RoleService, authz.ExecRun, true},
		{authz.RoleService, authz.VMDelete, false},
		{authz.RoleService, authz.VMCreate, false},
		{authz.RoleNode, authz.NodeManage, true},
		{authz.RoleAdmin, authz.NodeManage, false},
		{authz.Role("nope"), authz.VMGet, false},
	}
	for _, c := range cases {
		if got := c.role.Grants(c.perm); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestPrincipalProjectScoping(t *testing.T) {
	p := editorOn("p1")
	if !p.Can(authz.VMCreate, "p1") || p.Can(authz.VMCreate, "p2") || p.Can(authz.VMCreate, "") {
		t.Fatal("project binding must not leak to other projects or the global scope")
	}
	global := authz.Principal{Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleAdmin}}}
	if !global.Can(authz.ImageRegister, "") || !global.Can(authz.VMCreate, "anything") {
		t.Fatal("'*' binding applies everywhere including global")
	}
	if !p.Visible("p1") || p.Visible("p2") {
		t.Fatal("visibility")
	}
}

func TestRequireAndRequireResource(t *testing.T) {
	if err := authz.Require(context.Background(), authz.VMGet, "p1"); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("no principal: %v", err)
	}
	viewer := authz.Principal{Subject: "user:v", Bindings: []authz.Binding{{Project: "p1", Role: authz.RoleViewer}}}
	ctx := authz.WithPrincipal(context.Background(), &viewer)
	if err := authz.Require(ctx, authz.VMGet, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := authz.Require(ctx, authz.VMCreate, "p1"); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("want denied, got %v", err)
	}
	// resource in a project the caller cannot see looks exactly like a missing one
	if err := authz.RequireResource(ctx, authz.VMGet, "other", "vm", "vm_1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := authz.RequireResource(ctx, authz.VMDelete, "p1", "vm", "vm_1"); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("want denied, got %v", err)
	}
}

func TestTokenRoundTripAndRejections(t *testing.T) {
	iss, trusted := newIssuer(t, "k1")
	v := &authz.Verifier{Keys: map[string]authz.TrustedKey{"k1": trusted}, MaxTTL: time.Hour}

	tok, err := iss.Mint(editorOn("p1"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "user:robert" || !p.Can(authz.VMCreate, "p1") || p.TokenID == "" {
		t.Fatalf("bad principal %+v", p)
	}

	t.Run("expired", func(t *testing.T) {
		old := &authz.Issuer{KeyID: "k1", Key: iss.Key, Now: func() time.Time { return time.Now().Add(-2 * time.Hour) }}
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
		forger := &authz.Issuer{KeyID: "k1", Key: priv}
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
		rv := &authz.Verifier{Keys: v.Keys, Revocations: revoked{p.TokenID: true}}
		if _, err := rv.Verify(tok); err == nil {
			t.Fatal("revoked token accepted")
		}
	})
	t.Run("unknown role refused at mint", func(t *testing.T) {
		bad := authz.Principal{Subject: "x", Bindings: []authz.Binding{{Project: "p", Role: "root"}}}
		if _, err := iss.Mint(bad, time.Minute); err == nil {
			t.Fatal("minted a token with an unknown role")
		}
	})
}

type revoked map[string]bool

func (r revoked) Revoked(jti string) bool { return r[jti] }

func TestKeyRestrictsRoles(t *testing.T) {
	iss, trusted := newIssuer(t, "agentd")
	trusted.AllowedRoles = []authz.Role{authz.RoleService}
	v := &authz.Verifier{Keys: map[string]authz.TrustedKey{"agentd": trusted}}

	svc := authz.Principal{Subject: "svc:agentd", Type: authz.Service, Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleService}}}
	tok, _ := iss.Mint(svc, time.Minute)
	if _, err := v.Verify(tok); err != nil {
		t.Fatal(err)
	}
	evil := authz.Principal{Subject: "svc:agentd", Type: authz.Service, Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleAdmin}}}
	tok, _ = iss.Mint(evil, time.Minute)
	if _, err := v.Verify(tok); err == nil {
		t.Fatal("a compromised service key must not be able to mint admin tokens")
	}
}

func TestKeyFilesRoundTrip(t *testing.T) {
	pub, priv, _ := authz.GenerateKey()
	dir := t.TempDir()
	if err := authz.SavePrivateKey(filepath.Join(dir, "k.pem"), priv); err != nil {
		t.Fatal(err)
	}
	if err := authz.SavePublicKey(filepath.Join(dir, "k.pub"), pub); err != nil {
		t.Fatal(err)
	}
	p2, err := authz.LoadPrivateKey(filepath.Join(dir, "k.pem"))
	if err != nil || !p2.Equal(priv) {
		t.Fatalf("private key round trip: %v", err)
	}
	pub2, err := authz.LoadPublicKey(filepath.Join(dir, "k.pub"))
	if err != nil || !pub2.Equal(pub) {
		t.Fatalf("public key round trip: %v", err)
	}
	if _, err := authz.LoadPrivateKey(filepath.Join(dir, "k.pub")); err == nil {
		t.Fatal("public key file loaded as private key")
	}
}

// who echoes the authenticated subject back, so the interceptor can be tested end to end over HTTP.
type who struct {
	vmdv1connect.UnimplementedVMServiceHandler
}

func (who) GetVM(ctx context.Context, _ *connect.Request[vmdv1.GetVMRequest]) (*connect.Response[vmdv1.VM], error) {
	p, ok := authz.FromContext(ctx)
	if !ok {
		return nil, errs.ToConnect(errs.ErrUnauthenticated)
	}
	return connect.NewResponse(&vmdv1.VM{Name: p.Subject}), nil
}

func TestInterceptorOverHTTP(t *testing.T) {
	iss, trusted := newIssuer(t, "k1")
	v := &authz.Verifier{Keys: map[string]authz.TrustedKey{"k1": trusted}}
	mux := http.NewServeMux()
	mux.Handle(vmdv1connect.NewVMServiceHandler(who{}, connect.WithInterceptors(&authz.ServerInterceptor{Verifier: v})))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	call := func(src authz.TokenSource) error {
		opts := []connect.ClientOption{}
		if src != nil {
			opts = append(opts, connect.WithInterceptors(authz.ClientInterceptor(src)))
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
	if err := call(authz.StaticToken("garbage")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("garbage token: %v", err)
	}
	if err := call(authz.ServiceTokens(iss, editorOn("p1"), time.Minute)); err != nil {
		t.Fatalf("valid token: %v", err)
	}
}

func TestServiceTokensAreCached(t *testing.T) {
	iss, _ := newIssuer(t, "k1")
	now := time.Now()
	iss.Now = func() time.Time { return now }
	src := authz.ServiceTokens(iss, editorOn("p1"), time.Minute)
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
	if !authz.RoleOperator.Grants(authz.CapacityGet) || authz.RoleOperator.Grants(authz.VMGet) {
		t.Fatal("operators see capacity only, nothing inside projects")
	}
	for _, r := range []authz.Role{authz.RoleViewer, authz.RoleEditor, authz.RoleAdmin, authz.RoleService} {
		if !r.Grants(authz.VMMetrics) {
			t.Errorf("%s should read VM metrics", r)
		}
		if r.Grants(authz.CapacityGet) {
			t.Errorf("%s must not read cluster capacity", r)
		}
	}
	// capacity is global: a project-scoped operator binding must not satisfy it
	p := authz.Principal{Bindings: []authz.Binding{{Project: "p1", Role: authz.RoleOperator}}}
	if p.Can(authz.CapacityGet, "") {
		t.Fatal("capacity.get needs a '*' binding")
	}
	g := authz.Principal{Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleOperator}}}
	if !g.Can(authz.CapacityGet, "") {
		t.Fatal("'*' operator should see capacity")
	}
}

func TestRequireAnyProject(t *testing.T) {
	if err := authz.RequireAnyProject(context.Background(), authz.ImageGet); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("no principal: %v", err)
	}
	viewer := authz.Principal{Subject: "u", Bindings: []authz.Binding{{Project: "p1", Role: authz.RoleViewer}}}
	ctx := authz.WithPrincipal(context.Background(), &viewer)
	if err := authz.RequireAnyProject(ctx, authz.ImageGet); err != nil {
		t.Fatalf("viewer on p1 may read shared images: %v", err)
	}
	if err := authz.RequireAnyProject(ctx, authz.VMCreate); !errors.Is(err, errs.ErrPermissionDenied) {
		t.Fatalf("viewer may not create: %v", err)
	}
}

// Base64url decoding must be strict: flipping the unused trailing bits of the signature's last
// character may not produce a second valid spelling of the same token.
func TestTokenHasOneCanonicalForm(t *testing.T) {
	iss, trusted := newIssuer(t, "k1")
	v := &authz.Verifier{Keys: map[string]authz.TrustedKey{"k1": trusted}}
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
	if len(authz.Roles()) < 6 {
		t.Fatalf("roles: %v", authz.Roles())
	}
	for _, r := range authz.Roles() {
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

func TestFileRevocations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.txt")
	write := func(content string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	write("# revoked after the laptop theft\ntok_aaa\n\n  tok_bbb  # trailing comment\n", base)

	r, err := authz.NewFileRevocations(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Revoked("tok_aaa") || !r.Revoked("tok_bbb") || r.Revoked("tok_ccc") || r.Revoked("# revoked") || r.Len() != 2 {
		t.Fatalf("parsing: len=%d", r.Len())
	}
	if _, err := authz.NewFileRevocations(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file at start-up must be an error (a typo must not silently disable revocation)")
	}

	// The watcher picks up changes...
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Watch(ctx, 10*time.Millisecond, nil)
	write("tok_aaa\ntok_ccc\n", base.Add(time.Minute))
	deadline := time.Now().Add(3 * time.Second)
	for !r.Revoked("tok_ccc") {
		if time.Now().After(deadline) {
			t.Fatal("reload not noticed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Revoked("tok_bbb") {
		t.Fatal("an id removed from the file is no longer revoked")
	}

	// ...but a vanished file keeps the last good list in force.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if !r.Revoked("tok_aaa") || !r.Revoked("tok_ccc") {
		t.Fatal("a transient file problem must not un-revoke tokens")
	}
}

func TestVerifierRejectsRevokedTokenFromFile(t *testing.T) {
	iss, trusted := newIssuer(t, "k1")
	path := filepath.Join(t.TempDir(), "revoked.txt")
	tok, _ := iss.Mint(editorOn("p1"), time.Hour)
	v := &authz.Verifier{Keys: map[string]authz.TrustedKey{"k1": trusted}}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(p.TokenID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := authz.NewFileRevocations(path)
	if err != nil {
		t.Fatal(err)
	}
	v.Revocations = r
	if _, err := v.Verify(tok); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	other, _ := iss.Mint(editorOn("p1"), time.Hour)
	if _, err := v.Verify(other); err != nil {
		t.Fatalf("other tokens are unaffected: %v", err)
	}
}
