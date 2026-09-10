// Package network brings up a network interface natively (no busybox
// ip/udhcpc): rtnetlink for link/address/route, insomniacslk/dhcp for
// the DHCPv4 exchange -- and then keeps the resulting lease alive for
// as long as the process runs (see maintain).
package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/jsimonetti/rtnetlink/v2/rtnl"
	"golang.org/x/sys/unix"
)

// dhcpTimeout bounds a single DHCPv4 exchange (the initial DORA in Up,
// and each renewal or re-acquisition attempt in maintain) so boot
// doesn't hang forever waiting for a server that never answers.
const dhcpTimeout = 15 * time.Second

// renewRetryMin is the floor on how long maintain waits between failed
// renewal attempts. RFC 2131 section 4.4.5 has the client retry at
// half the remaining lease time but never more often than once a
// minute, so a server that's down doesn't get hammered as the lease
// runs out.
const renewRetryMin = time.Minute

// reacquireRetry is how long maintain waits between failed attempts to
// obtain a brand-new lease after the old one expired. There's no lease
// left to pace against at that point; this just keeps the DISCOVER
// broadcasts to a sane rate.
const reacquireRetry = 30 * time.Second

// infiniteLease is the DHCP lease-time value (RFC 2132 section 9.2) a
// server uses for a lease that never expires.
const infiniteLease = 0xffffffff * time.Second

// hostnameOption is sent to the DHCP server as option 12 (hostname).
const hostnameOption = "openbao"

// dhcpModifiers are applied to every DHCP message this package sends
// (discover, request, and renewal alike), so a renewed lease asks for
// the same options the original did.
var dhcpModifiers = []dhcpv4.Modifier{
	dhcpv4.WithRequestedOptions(
		dhcpv4.OptionSubnetMask,
		dhcpv4.OptionRouter,
		dhcpv4.OptionDomainNameServer,
	),
	dhcpv4.WithOption(dhcpv4.OptHostName(hostnameOption)),
}

// config is the host-side state a lease translates into: the address
// to assign, the default gateway (nil when the server offered none),
// and the DNS servers to write to /etc/resolv.conf. maintain compares
// the config of a renewed lease against the one currently applied to
// decide whether the kernel needs touching at all.
type config struct {
	addr *net.IPNet
	gw   net.IP
	dns  []net.IP
}

// Up brings iface up and configures it via DHCPv4: link up, address,
// default route (when the server offers a gateway), and DNS
// (/etc/resolv.conf from the lease's nameservers). It then starts a
// goroutine (maintain) that renews the lease before it expires and
// re-acquires one if it does, for as long as ctx lives.
//
// Failures are returned as errors but are treated as non-fatal by the
// Boot caller — the appliance boots into a network-less state rather
// than refusing to start. Install, which needs the network to fetch
// its boot artifact, treats them as fatal.
func Up(ctx context.Context, iface string) error {
	conn, err := rtnl.Dial(nil)
	if err != nil {
		return fmt.Errorf("dialing rtnetlink: %w", err)
	}
	defer func() { _ = conn.Close() }()

	link, err := findLink(conn, iface)
	if err != nil {
		return err
	}

	if err := conn.LinkUp(link); err != nil {
		return fmt.Errorf("bringing up %s: %w", iface, err)
	}

	dhcpCtx, cancel := context.WithTimeout(ctx, dhcpTimeout)
	defer cancel()

	lease, err := requestLease(dhcpCtx, iface)
	if err != nil {
		return fmt.Errorf("DHCP on %s: %w", iface, err)
	}

	cfg := leaseConfig(lease.ACK)

	if err := apply(conn, link, iface, cfg); err != nil {
		return err
	}

	go maintain(ctx, iface, lease, cfg)

	return nil
}

// leaseConfig extracts the config a lease's ACK describes. A missing
// subnet mask defaults to /24, as before.
func leaseConfig(ack *dhcpv4.DHCPv4) config {
	mask := ack.SubnetMask()
	if mask == nil {
		mask = net.CIDRMask(24, 32)
	}

	cfg := config{
		addr: &net.IPNet{IP: ack.YourIPAddr, Mask: mask},
		dns:  ack.DNS(),
	}

	if routers := ack.Router(); len(routers) > 0 {
		cfg.gw = routers[0]
	}

	return cfg
}

// configChanged reports whether two configs differ in any way the
// kernel or resolv.conf would need updating for.
func configChanged(a, b config) bool {
	if !a.addr.IP.Equal(b.addr.IP) || a.addr.Mask.String() != b.addr.Mask.String() {
		return true
	}

	if !a.gw.Equal(b.gw) {
		return true
	}

	if len(a.dns) != len(b.dns) {
		return true
	}

	for i := range a.dns {
		if !a.dns[i].Equal(b.dns[i]) {
			return true
		}
	}

	return false
}

// apply configures iface per cfg: address, default route (via
// RouteReplace, so re-applying after a gateway change doesn't trip
// over the old route), and resolv.conf.
//
// It is idempotent with respect to the address: an EEXIST from the
// kernel means the address is already there, which is the desired end
// state, not a failure. That matters for maintain's retry loops -- if
// a previous apply got the address on but then failed at a later step
// (a lease with no DNS servers, say), the retry must be able to make
// progress instead of failing on the address forever.
func apply(conn *rtnl.Conn, link *net.Interface, iface string, cfg config) error {
	if err := conn.AddrAdd(link, cfg.addr); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("assigning address %s to %s: %w", cfg.addr, iface, err)
	}

	fmt.Printf("[net] %s: address %s\n", iface, cfg.addr)

	if cfg.gw != nil {
		if err := conn.RouteReplace(link, defaultRoute(), cfg.gw); err != nil {
			return fmt.Errorf("adding default route via %s on %s: %w", cfg.gw, iface, err)
		}

		fmt.Printf("[net] %s: default route via %s\n", iface, cfg.gw)
	}

	if err := writeResolvConf(cfg.dns); err != nil {
		return fmt.Errorf("configuring DNS: %w", err)
	}

	fmt.Printf("[net] %s: DNS %s\n", iface, joinIPs(cfg.dns))

	return nil
}

// unapply removes what apply configured on the kernel side: the
// default route (if cfg had a gateway) and the address. resolv.conf is
// left alone -- a stale nameserver is harmless until a new lease
// overwrites it, whereas an address the lease no longer covers must
// not stay in use (RFC 2131 section 4.4.5). Both removals are
// best-effort and only logged: deleting the address also drops the
// routes that depended on it, so the route deletion frequently finds
// nothing left to delete.
func unapply(conn *rtnl.Conn, link *net.Interface, iface string, cfg config) {
	if cfg.gw != nil {
		if err := conn.RouteDel(link, defaultRoute()); err != nil {
			fmt.Printf("[net] %s: removing default route via %s: %v\n", iface, cfg.gw, err)
		}
	}

	if err := conn.AddrDel(link, cfg.addr); err != nil {
		fmt.Printf("[net] %s: removing address %s: %v\n", iface, cfg.addr, err)
	} else {
		fmt.Printf("[net] %s: released address %s\n", iface, cfg.addr)
	}
}

// defaultRoute is the 0.0.0.0/0 destination.
func defaultRoute() net.IPNet {
	return net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
}

// timers are the two points in a lease's life maintain acts on,
// derived from the ACK by leaseTimers.
//
// RFC 2131 also defines T2 (rebinding: stop unicasting to the original
// server and broadcast to any server), but nclient4's Renew already
// sends every renewal as a broadcast, so there is no distinct
// rebinding behaviour to switch to here and T2 is not tracked.
type timers struct {
	// renew is when to start trying to renew (T1).
	renew time.Time
	// expiry is when the lease ends and the address must be dropped
	// if no renewal succeeded.
	expiry time.Time
}

// leaseTimers computes the renewal and expiry times for a lease
// acquired at created, per RFC 2131 section 4.4.5: T1 from option 58
// if the server sent one, else half the lease; expiry from option 51.
// A T1 that isn't strictly inside the lease is ignored in favour of the
// default. ok is false when the lease never expires (either an
// explicitly infinite lease time, or no lease time at all, which a
// compliant server never omits from an ACK and which there is nothing
// sensible to renew against).
func leaseTimers(ack *dhcpv4.DHCPv4, created time.Time) (t timers, ok bool) {
	lease := ack.IPAddressLeaseTime(0)
	if lease <= 0 || lease >= infiniteLease {
		return timers{}, false
	}

	renew := ack.IPAddressRenewalTime(lease / 2)
	if renew <= 0 || renew >= lease {
		renew = lease / 2
	}

	return timers{
		renew:  created.Add(renew),
		expiry: created.Add(lease),
	}, true
}

// maintain keeps lease alive on iface for as long as ctx lives. The
// address a DHCP server hands out is only ours for the lease time it
// names; once that runs out the server is free to reassign it, and an
// appliance that carried on using it would silently become
// unreachable or collide with another host. So: at T1 start renewing,
// retrying with backoff; if the lease expires anyway, drop the address
// and go back to a full DISCOVER until a server answers. A renewed
// lease that changes the address, gateway, or DNS is applied to the
// kernel; one that changes nothing (the common case) touches nothing.
//
// It runs as a goroutine that is never explicitly stopped -- Install's
// caller reboots and Boot's runs until power-off -- so ctx is only
// checked for the sake of tests and any future caller that wants to
// take an interface down cleanly.
func maintain(ctx context.Context, iface string, lease *nclient4.Lease, cfg config) {
	for {
		t, ok := leaseTimers(lease.ACK, lease.CreationTime)
		if !ok {
			fmt.Printf("[net] %s: lease does not expire, not renewing\n", iface)

			return
		}

		fmt.Printf("[net] %s: lease expires %s, renewing from %s\n", iface, t.expiry.Format(time.RFC3339), t.renew.Format(time.RFC3339))

		if !sleepUntil(ctx, t.renew) {
			return
		}

		if renewed, ok := renewUntil(ctx, iface, lease, t.expiry); ok {
			newCfg := leaseConfig(renewed.ACK)

			if configChanged(cfg, newCfg) {
				fmt.Printf("[net] %s: lease renewed with new configuration\n", iface)
				reconfigure(iface, cfg, newCfg)
			} else {
				fmt.Printf("[net] %s: lease renewed\n", iface)
			}

			lease, cfg = renewed, newCfg

			continue
		}

		if ctx.Err() != nil {
			return
		}

		fmt.Printf("[net] %s: lease expired without renewal, releasing address\n", iface)

		if err := withLink(iface, func(conn *rtnl.Conn, link *net.Interface) error {
			unapply(conn, link, iface, cfg)

			return nil
		}); err != nil {
			fmt.Printf("[net] %s: %v\n", iface, err)
		}

		lease, cfg, ok = reacquire(ctx, iface)
		if !ok {
			return
		}
	}
}

// renewUntil tries to renew lease until it succeeds or expiry passes,
// pacing retries per renewRetryMin. It returns ok=false on expiry or
// ctx cancellation. Each attempt opens and closes its own DHCP client:
// the raw AF_PACKET socket nclient4 uses sees every DHCP frame on the
// interface, and there's no reason to hold one open for the hours
// between renewals.
func renewUntil(ctx context.Context, iface string, lease *nclient4.Lease, expiry time.Time) (*nclient4.Lease, bool) {
	for {
		remaining := time.Until(expiry)
		if remaining <= 0 || ctx.Err() != nil {
			return nil, false
		}

		renewed, err := renewOnce(ctx, iface, lease, min(dhcpTimeout, remaining))
		if err == nil {
			return renewed, true
		}

		fmt.Printf("[net] %s: renewing lease: %v\n", iface, err)

		// Retry at half the remaining time, but not more often than
		// renewRetryMin, and never past expiry itself.
		remaining = time.Until(expiry)
		wait := min(max(remaining/2, renewRetryMin), remaining)

		if wait <= 0 || !sleepUntil(ctx, time.Now().Add(wait)) {
			return nil, false
		}
	}
}

// renewOnce performs a single renewal exchange bounded by timeout.
func renewOnce(ctx context.Context, iface string, lease *nclient4.Lease, timeout time.Duration) (*nclient4.Lease, error) {
	client, err := nclient4.New(iface)
	if err != nil {
		return nil, fmt.Errorf("creating DHCP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return client.Renew(attemptCtx, lease, dhcpModifiers...)
}

// reacquire obtains and applies a brand-new lease after the previous
// one expired, retrying every reacquireRetry until a server answers or
// ctx is canceled (ok=false).
func reacquire(ctx context.Context, iface string) (lease *nclient4.Lease, cfg config, ok bool) {
	for {
		if ctx.Err() != nil {
			return nil, config{}, false
		}

		lease, cfg, err := acquireAndApply(ctx, iface)
		if err == nil {
			fmt.Printf("[net] %s: new lease acquired\n", iface)

			return lease, cfg, true
		}

		fmt.Printf("[net] %s: acquiring new lease: %v\n", iface, err)

		if !sleepUntil(ctx, time.Now().Add(reacquireRetry)) {
			return nil, config{}, false
		}
	}
}

// acquireAndApply is one full DORA exchange followed by apply.
func acquireAndApply(ctx context.Context, iface string) (*nclient4.Lease, config, error) {
	dhcpCtx, cancel := context.WithTimeout(ctx, dhcpTimeout)
	defer cancel()

	lease, err := requestLease(dhcpCtx, iface)
	if err != nil {
		return nil, config{}, fmt.Errorf("DHCP: %w", err)
	}

	cfg := leaseConfig(lease.ACK)

	err = withLink(iface, func(conn *rtnl.Conn, link *net.Interface) error {
		return apply(conn, link, iface, cfg)
	})
	if err != nil {
		return nil, config{}, err
	}

	return lease, cfg, nil
}

// reconfigure swaps the kernel and resolv.conf state from old to
// updated after a renewal changed something. Failures are logged, not
// returned: there's no caller that can do better than try again at the
// next renewal, and maintain records updated as current regardless so
// it compares against what the server most recently said.
func reconfigure(iface string, old, updated config) {
	err := withLink(iface, func(conn *rtnl.Conn, link *net.Interface) error {
		unapply(conn, link, iface, old)

		return apply(conn, link, iface, updated)
	})
	if err != nil {
		fmt.Printf("[net] %s: applying renewed lease: %v\n", iface, err)
	}
}

// withLink dials rtnetlink, looks up iface, runs fn, and closes the
// connection. maintain only touches the kernel on rare events (a
// changed lease, an expiry), so a fresh connection per event is
// simpler than keeping one open for the life of the process.
func withLink(iface string, fn func(*rtnl.Conn, *net.Interface) error) error {
	conn, err := rtnl.Dial(nil)
	if err != nil {
		return fmt.Errorf("dialing rtnetlink: %w", err)
	}
	defer func() { _ = conn.Close() }()

	link, err := findLink(conn, iface)
	if err != nil {
		return err
	}

	return fn(conn, link)
}

// sleepUntil blocks until t or ctx is done, reporting which (true for
// t). A t already in the past returns immediately.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// writeResolvConf writes servers as /etc/resolv.conf nameserver lines
// so subsequent name lookups use the DHCP-offered DNS servers.
func writeResolvConf(servers []net.IP) error {
	if len(servers) == 0 {
		return errors.New("DHCP offered no DNS servers")
	}

	var b strings.Builder
	for _, ns := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}

	if err := os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing /etc/resolv.conf: %w", err)
	}

	return nil
}

func joinIPs(ips []net.IP) string {
	parts := make([]string, len(ips))
	for i, ip := range ips {
		parts[i] = ip.String()
	}

	return strings.Join(parts, ", ")
}

func findLink(conn *rtnl.Conn, name string) (*net.Interface, error) {
	links, err := conn.Links()
	if err != nil {
		return nil, fmt.Errorf("listing links: %w", err)
	}

	for _, link := range links {
		if link.Name == name {
			return link, nil
		}
	}

	return nil, fmt.Errorf("link %q not found", name)
}

func requestLease(ctx context.Context, iface string) (*nclient4.Lease, error) {
	client, err := nclient4.New(iface)
	if err != nil {
		return nil, fmt.Errorf("creating DHCP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	return client.Request(ctx, dhcpModifiers...)
}
