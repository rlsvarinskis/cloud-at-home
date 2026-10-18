package qemudriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// newFirewallCmd returns a fake host command runner for firewall tests.
func newFirewallCmd() *fakeCmd {
	procs := newFakeProcs()
	return newFakeCmd(procs, newFakeGuests(procs))
}

// countCalls counts the recorded commands that start with prefix.
func countCalls(c *fakeCmd, prefix string) int {
	return len(c.commandsNamed(prefix))
}

// indexOf returns the position of the first recorded command equal to line, or -1.
func indexOf(c *fakeCmd, line string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, l := range c.calls {
		if l == line {
			return i
		}
	}
	return -1
}

func TestLockDownNetworkInstallsRules(t *testing.T) {
	c := newFirewallCmd()
	if err := LockDownNetwork(context.Background(), c, "qemu", "127.0.0.11", "2001:db8::53"); err != nil {
		t.Fatalf("LockDownNetwork: %v", err)
	}
	for _, line := range []string{
		"iptables -N SBX",
		"iptables -F SBX",
		"iptables -A SBX -d 127.0.0.11 -j ACCEPT",
		"iptables -A SBX -d 10.0.0.0/8 -j REJECT",
		"iptables -A SBX -d 172.16.0.0/12 -j REJECT",
		"iptables -A SBX -d 192.168.0.0/16 -j REJECT",
		"iptables -A SBX -d 169.254.0.0/16 -j REJECT",
		"iptables -A SBX -d 100.64.0.0/10 -j REJECT",
		"iptables -A SBX -d 127.0.0.0/8 -j REJECT",
		"ip6tables -A SBX -d 2001:db8::53 -j ACCEPT",
		"ip6tables -A SBX -d ::1/128 -j REJECT",
		"ip6tables -A SBX -d fc00::/7 -j REJECT",
		"ip6tables -A SBX -d fe80::/10 -j REJECT",
		"iptables -A OUTPUT -m owner --uid-owner qemu -m conntrack --ctstate NEW -j SBX",
		"ip6tables -A OUTPUT -m owner --uid-owner qemu -m conntrack --ctstate NEW -j SBX",
	} {
		if indexOf(c, line) < 0 {
			t.Errorf("missing command %q", line)
		}
	}
	// The resolver accepts must come before the rejects, or DNS would be refused.
	if accept, reject := indexOf(c, "iptables -A SBX -d 127.0.0.11 -j ACCEPT"), indexOf(c, "iptables -A SBX -d 10.0.0.0/8 -j REJECT"); accept < 0 || reject < 0 || accept > reject {
		t.Errorf("resolver accept (%d) must precede the rejects (%d)", accept, reject)
	}
	// Each address family goes to its own tool.
	if indexOf(c, "iptables -A SBX -d 2001:db8::53 -j ACCEPT") >= 0 {
		t.Error("an IPv6 resolver was added to the IPv4 chain")
	}
}

func TestLockDownNetworkIsIdempotent(t *testing.T) {
	c := newFirewallCmd()
	ctx := context.Background()
	if err := LockDownNetwork(ctx, c, "qemu"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := LockDownNetwork(ctx, c, "qemu"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := countCalls(c, "iptables -A OUTPUT"); n != 1 {
		t.Errorf("IPv4 jump installed %d times, want 1", n)
	}
	if n := countCalls(c, "ip6tables -A OUTPUT"); n != 1 {
		t.Errorf("IPv6 jump installed %d times, want 1", n)
	}
	// The second run must not have needed to create the chain again.
	if n := countCalls(c, "iptables -N SBX"); n != 1 {
		t.Errorf("chain created %d times, want once", n)
	}
	// Each run checks the jump once per family. The second run finds it in place and adds nothing.
	if n := countCalls(c, "iptables -C OUTPUT") + countCalls(c, "ip6tables -C OUTPUT"); n != 4 {
		t.Errorf("jump checks = %d, want 4 (two runs, two families)", n)
	}
}

func TestLockDownNetworkWithoutAllowListHasNoAccepts(t *testing.T) {
	c := newFirewallCmd()
	if err := LockDownNetwork(context.Background(), c, "qemu"); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(c, "iptables -A SBX -d"); n == 0 {
		t.Fatal("no IPv4 rules installed")
	}
	for _, line := range c.commandsNamed("iptables -A SBX") {
		if strings.HasSuffix(line, "ACCEPT") {
			t.Errorf("unexpected accept rule %q", line)
		}
	}
}

func TestLockDownNetworkRejectsBadInput(t *testing.T) {
	c := newFirewallCmd()
	if err := LockDownNetwork(context.Background(), c, ""); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("empty user: err = %v, want invalid", err)
	}
	if err := LockDownNetwork(context.Background(), c, "qemu", "not-an-address"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("bad resolver: err = %v, want invalid", err)
	}
	if len(c.calls) != 0 {
		t.Errorf("commands ran for rejected input: %v", c.calls)
	}
}

func TestLockDownNetworkReportsFailure(t *testing.T) {
	c := newFirewallCmd()
	c.fail["-F SBX"] = errors.New("iptables: permission denied")
	if err := LockDownNetwork(context.Background(), c, "qemu"); err == nil || !strings.Contains(err.Error(), "flush") {
		t.Errorf("err = %v, want a flush failure", err)
	}
	if indexOf(c, "iptables -A OUTPUT -m owner --uid-owner qemu -m conntrack --ctstate NEW -j SBX") >= 0 {
		t.Error("the jump was installed after the chain failed to flush")
	}
}
