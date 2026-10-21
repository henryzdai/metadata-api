package access

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

func ParseListenAddress(address string) (string, error) {
	host, portText, err := net.SplitHostPort(address)
	port, ok := parsePort(portText)
	if err != nil || !ok {
		return "", errors.New("listen address must use loopback and a numeric port from 0 to 65535")
	}
	if strings.HasPrefix(address, "[") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() {
			return "", errors.New("listen address has an invalid bracketed host")
		}
	}
	normalized, ok := loopbackHost(host)
	if !ok {
		return "", errors.New("listen address must use literal loopback or localhost")
	}
	return net.JoinHostPort(normalized, strconv.Itoa(port)), nil
}

type Authority struct{ port int }

func NewAuthority(address net.Addr) (Authority, error) {
	tcp, ok := address.(*net.TCPAddr)
	if !ok || tcp == nil || tcp.Zone != "" || tcp.Port < 1 || tcp.Port > 65535 {
		return Authority{}, errors.New("a bound loopback TCP authority is required")
	}
	ip, ok := netip.AddrFromSlice(tcp.IP)
	if !ok || !ip.Unmap().IsLoopback() {
		return Authority{}, errors.New("a bound loopback TCP authority is required")
	}
	return Authority{port: tcp.Port}, nil
}

func (a Authority) Valid() bool { return a.port >= 1 && a.port <= 65535 }

func (a Authority) Accepts(authority string) bool {
	if !a.Valid() {
		return false
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil {
		// An omitted port denotes 80 for this HTTP-only listener.
		if a.port != 80 {
			return false
		}
		host = authority
		if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
			host = authority[1 : len(authority)-1]
		} else if strings.ContainsAny(authority, "[]:") {
			return false
		}
		portText = "80"
	}
	port, ok := parsePort(portText)
	if !ok || port != a.port {
		return false
	}
	if strings.HasPrefix(authority, "[") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() {
			return false
		}
	}
	_, ok = loopbackHost(host)
	return ok
}

func loopbackHost(host string) (string, bool) {
	if strings.EqualFold(host, "localhost") {
		return "127.0.0.1", true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.Unmap().IsLoopback() {
		return "", false
	}
	return ip.Unmap().String(), true
}

func parsePort(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	port, err := strconv.Atoi(value)
	return port, err == nil && port <= 65535
}
