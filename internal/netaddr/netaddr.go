// Package netaddr resolves the address to bind and advertise in the pairing
// URL (even-terminal's resolveHost, plus a loopback mode).
package netaddr

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Host is the advertised address.
type Host struct {
	Label   string // LAN | Tailscale | <interface> | Loopback
	Address string // "" = loopback only (no LAN address found)
}

var private = []*net.IPNet{mustCIDR("10.0.0.0/8"), mustCIDR("192.168.0.0/16"), mustCIDR("172.16.0.0/12")}
var tailnet = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func ipv4s(iface net.Interface) []net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil {
			out = append(out, ip4)
		}
	}
	return out
}

// LANAddress is the first non-loopback private IPv4, in interface order.
func LANAddress() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		for _, ip := range ipv4s(iface) {
			for _, n := range private {
				if n.Contains(ip) {
					return ip.String()
				}
			}
		}
	}
	return ""
}

func tailscaleAddress() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output(); err == nil {
		if first := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]); first != "" {
			return first
		}
	}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		for _, ip := range ipv4s(iface) {
			if tailnet.Contains(ip) {
				return ip.String()
			}
		}
	}
	return ""
}

// Resolve maps a bind mode to the advertised host.
func Resolve(bind string) (Host, error) {
	switch {
	case bind == "loopback":
		return Host{Label: "Loopback", Address: "127.0.0.1"}, nil
	case bind == "tailscale":
		ip := tailscaleAddress()
		if ip == "" {
			return Host{}, fmt.Errorf("failed to get Tailscale IPv4 address (is tailscale installed and running?)")
		}
		return Host{Label: "Tailscale", Address: ip}, nil
	case strings.HasPrefix(bind, "interface:"):
		name := strings.TrimPrefix(bind, "interface:")
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return Host{}, fmt.Errorf("interface %q: %w", name, err)
		}
		ips := ipv4s(*iface)
		if len(ips) == 0 {
			return Host{}, fmt.Errorf("failed to get IPv4 address for interface %q", name)
		}
		return Host{Label: name, Address: ips[0].String()}, nil
	default:
		return Host{Label: "LAN", Address: LANAddress()}, nil
	}
}

// Expose is /api/info extra.expose for a bind mode.
func Expose(bind string) string {
	switch {
	case bind == "tailscale":
		return "tailscale"
	case strings.HasPrefix(bind, "interface:"):
		return "other"
	}
	return "off"
}
