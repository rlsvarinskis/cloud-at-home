package qemudriver

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// sandboxChain is the user chain that holds the VM user's rules, in both iptables and ip6tables.
const sandboxChain = "SBX"

// family is the rule set of one address family.
type family struct {
	tool   string   // iptables or ip6tables
	allow  []string // resolver addresses accepted before the rejects
	reject []string // destination networks that new connections may not reach
}

// LockDownNetwork stops the VM user from opening NEW connections to private, loopback, link-local
// and CGNAT networks. QEMU's user-mode NAT runs as runAsUser, so without this a guest could reach
// the host's own network. The rules are idempotent: they are rebuilt on each call, and the jump from
// OUTPUT is added only when it is missing.
//
// allowDNS lists the resolver addresses the VM user may still reach, because the QEMU process
// resolves guest DNS through them. Addresses are sorted by family. Callers must run this before any
// VM is started, and they must treat its failure as fatal.
//
// The rules are rebuilt by flushing the chain and refilling it. For a moment during a rebuild the
// chain is empty, which allows traffic, so call it only at startup, before VMs are running.
func LockDownNetwork(ctx context.Context, cmd Commander, runAsUser string, allowDNS ...string) error {
	if runAsUser == "" {
		return errs.Invalid("firewall: the VM user must be named")
	}
	v4 := family{
		tool: "iptables",
		reject: []string{
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
			"169.254.0.0/16", "100.64.0.0/10", "127.0.0.0/8",
		},
	}
	v6 := family{
		tool:   "ip6tables",
		reject: []string{"::1/128", "fc00::/7", "fe80::/10"},
	}
	for _, a := range allowDNS {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return errs.Invalid("firewall: DNS resolver %q: %v", a, err)
		}
		if ip.Is4() {
			v4.allow = append(v4.allow, ip.String())
		} else {
			v6.allow = append(v6.allow, ip.String())
		}
	}
	for _, f := range []family{v4, v6} {
		if err := lockFamily(ctx, cmd, f, runAsUser); err != nil {
			return err
		}
	}
	return nil
}

// lockFamily installs the sandbox chain for one address family and jumps to it from OUTPUT.
func lockFamily(ctx context.Context, cmd Commander, f family, user string) error {
	ipt := func(args ...string) error {
		_, err := cmd.Run(ctx, f.tool, args...)
		return err
	}
	// -L on a missing chain fails, so a failure here means the chain must be created.
	if err := ipt("-nL", sandboxChain); err != nil {
		if err := ipt("-N", sandboxChain); err != nil {
			return fmt.Errorf("firewall: %s create chain %s: %w", f.tool, sandboxChain, err)
		}
	}
	if err := ipt("-F", sandboxChain); err != nil {
		return fmt.Errorf("firewall: %s flush chain %s: %w", f.tool, sandboxChain, err)
	}
	for _, a := range f.allow {
		if err := ipt("-A", sandboxChain, "-d", a, "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("firewall: %s accept %s: %w", f.tool, a, err)
		}
	}
	for _, n := range f.reject {
		if err := ipt("-A", sandboxChain, "-d", n, "-j", "REJECT"); err != nil {
			return fmt.Errorf("firewall: %s reject %s: %w", f.tool, n, err)
		}
	}

	jump := []string{"OUTPUT", "-m", "owner", "--uid-owner", user,
		"-m", "conntrack", "--ctstate", "NEW", "-j", sandboxChain}
	if err := ipt(append([]string{"-C"}, jump...)...); err == nil {
		return nil // the jump is already in place
	}
	if err := ipt(append([]string{"-A"}, jump...)...); err != nil {
		return fmt.Errorf("firewall: %s add jump: %w", f.tool, err)
	}
	return nil
}
