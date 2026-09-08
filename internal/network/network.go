// Package network brings up a network interface natively (no busybox
// ip/udhcpc): rtnetlink for link/address/route, insomniacslk/dhcp for
// the DHCPv4 exchange.
package network

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/jsimonetti/rtnetlink/v2/rtnl"
)

// dhcpTimeout bounds the DHCPv4 exchange so boot doesn't hang forever
// waiting for a server that never answers.
const dhcpTimeout = 15 * time.Second

// hostnameOption is sent to the DHCP server as option 12 (hostname).
const hostnameOption = "openbao"

// Up brings iface up and configures it via DHCPv4: link up, address, and
// default route (when the server offers a gateway).
//
// Failures are returned as errors but are treated as non-fatal by the
// caller — the appliance boots into a network-less state rather than
// refusing to start.
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

	ack := lease.ACK

	mask := ack.SubnetMask()
	if mask == nil {
		// Server didn't offer a subnet mask; default to a /24.
		mask = net.CIDRMask(24, 32)
	}

	addr := &net.IPNet{
		IP:   ack.YourIPAddr,
		Mask: mask,
	}

	if err := conn.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("assigning address %s to %s: %w", addr, iface, err)
	}

	fmt.Printf("[net] %s: address %s\n", iface, addr)

	if routers := ack.Router(); len(routers) > 0 {
		gw := routers[0]

		_, defaultRoute, err := net.ParseCIDR("0.0.0.0/0")
		if err != nil {
			return fmt.Errorf("parsing default route destination: %w", err)
		}

		if err := conn.RouteAdd(link, *defaultRoute, gw); err != nil {
			return fmt.Errorf("adding default route via %s on %s: %w", gw, iface, err)
		}

		fmt.Printf("[net] %s: default route via %s\n", iface, gw)
	}

	return nil
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

	return client.Request(ctx,
		dhcpv4.WithRequestedOptions(
			dhcpv4.OptionSubnetMask,
			dhcpv4.OptionRouter,
			dhcpv4.OptionDomainNameServer,
		),
		dhcpv4.WithOption(dhcpv4.OptHostName(hostnameOption)),
	)
}
