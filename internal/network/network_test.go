package network

import (
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func ack(t *testing.T, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()

	p, err := dhcpv4.New(append([]dhcpv4.Modifier{dhcpv4.WithMessageType(dhcpv4.MessageTypeAck)}, mods...)...)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func TestLeaseTimers(t *testing.T) {
	created := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		mods       []dhcpv4.Modifier
		wantOK     bool
		wantRenew  time.Duration
		wantExpiry time.Duration
	}{
		{
			name:       "defaults: T1 is half the lease",
			mods:       []dhcpv4.Modifier{dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour))},
			wantOK:     true,
			wantRenew:  30 * time.Minute,
			wantExpiry: time.Hour,
		},
		{
			name: "server-supplied T1 wins",
			mods: []dhcpv4.Modifier{
				dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour)),
				dhcpv4.WithOption(dhcpv4.OptRenewTimeValue(10 * time.Minute)),
			},
			wantOK:     true,
			wantRenew:  10 * time.Minute,
			wantExpiry: time.Hour,
		},
		{
			name: "T1 outside the lease falls back to half",
			mods: []dhcpv4.Modifier{
				dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour)),
				dhcpv4.WithOption(dhcpv4.OptRenewTimeValue(2 * time.Hour)),
			},
			wantOK:     true,
			wantRenew:  30 * time.Minute,
			wantExpiry: time.Hour,
		},
		{
			name:   "infinite lease never renews",
			mods:   []dhcpv4.Modifier{dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(infiniteLease))},
			wantOK: false,
		},
		{
			name:   "no lease time never renews",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := leaseTimers(ack(t, tc.mods...), created)

			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}

			if !ok {
				return
			}

			if want := created.Add(tc.wantRenew); !got.renew.Equal(want) {
				t.Errorf("renew = %s, want %s", got.renew, want)
			}

			if want := created.Add(tc.wantExpiry); !got.expiry.Equal(want) {
				t.Errorf("expiry = %s, want %s", got.expiry, want)
			}
		})
	}
}

func TestLeaseConfig_And_ConfigChanged(t *testing.T) {
	base := []dhcpv4.Modifier{
		dhcpv4.WithYourIP(net.IPv4(10, 0, 2, 15)),
		dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))),
		dhcpv4.WithOption(dhcpv4.OptRouter(net.IPv4(10, 0, 2, 2))),
		dhcpv4.WithOption(dhcpv4.OptDNS(net.IPv4(10, 0, 2, 3))),
	}

	orig := leaseConfig(ack(t, base...))

	if orig.addr.String() != "10.0.2.15/24" {
		t.Fatalf("addr = %s", orig.addr)
	}

	if !orig.gw.Equal(net.IPv4(10, 0, 2, 2)) {
		t.Fatalf("gw = %s", orig.gw)
	}

	if len(orig.dns) != 1 || !orig.dns[0].Equal(net.IPv4(10, 0, 2, 3)) {
		t.Fatalf("dns = %v", orig.dns)
	}

	// The same lease renewed: nothing to do.
	if configChanged(orig, leaseConfig(ack(t, base...))) {
		t.Fatal("identical lease reported as changed")
	}

	changes := map[string]dhcpv4.Modifier{
		"address": dhcpv4.WithYourIP(net.IPv4(10, 0, 2, 16)),
		"mask":    dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(16, 32))),
		"gateway": dhcpv4.WithOption(dhcpv4.OptRouter(net.IPv4(10, 0, 2, 1))),
		"dns":     dhcpv4.WithOption(dhcpv4.OptDNS(net.IPv4(10, 0, 2, 3), net.IPv4(8, 8, 8, 8))),
	}

	for name, mod := range changes {
		t.Run(name, func(t *testing.T) {
			// Later modifiers override earlier ones for the same option.
			if !configChanged(orig, leaseConfig(ack(t, append(append([]dhcpv4.Modifier{}, base...), mod)...))) {
				t.Fatalf("%s change not detected", name)
			}
		})
	}

	// No mask offered: defaults to /24, and dropping the router makes
	// gw nil, which must also count as a change.
	noGW := leaseConfig(ack(t,
		dhcpv4.WithYourIP(net.IPv4(10, 0, 2, 15)),
		dhcpv4.WithOption(dhcpv4.OptDNS(net.IPv4(10, 0, 2, 3))),
	))

	if noGW.addr.String() != "10.0.2.15/24" || noGW.gw != nil {
		t.Fatalf("no-mask/no-router config = %+v", noGW)
	}

	if !configChanged(orig, noGW) {
		t.Fatal("lost gateway not detected as a change")
	}
}
