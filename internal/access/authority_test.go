package access

import (
	"net"
	"strings"
	"testing"
)

func TestListenAddressesNeverResolveArbitraryNamesOrExposeTheListener(t *testing.T) {
	for _, test := range []struct{ supplied, want string }{
		{"127.0.0.1:0", "127.0.0.1:0"},
		{"127.1.2.3:65535", "127.1.2.3:65535"},
		{"[::1]:8080", "[::1]:8080"},
		{"[0:0:0:0:0:0:0:1]:0", "[::1]:0"},
		{"[::ffff:127.0.0.1]:8080", "127.0.0.1:8080"},
		{"localhost:0", "127.0.0.1:0"},
		{"LOCALHOST:1234", "127.0.0.1:1234"},
	} {
		address, err := ParseListenAddress(test.supplied)
		if err != nil || address != test.want {
			t.Fatalf("local listen address %q normalized to %q, error %v", test.supplied, address, err)
		}
	}
	for _, address := range []string{
		"", ":8080", "0.0.0.0:8080", "[::]:8080", "*:8080", "192.0.2.1:8080",
		"[2001:db8::1]:8080", "example.invalid:8080", "localhost.example:8080", "localhost.:8080",
		"user@localhost:8080", "[localhost]:8080", "[127.0.0.1]:8080",
		"127.0.0.1", "::1:8080", "[::1%loopback]:8080",
		"127.0.0.1:", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1:+1",
		"127.0.0.1:http", "127.0.0.1: 8080", " 127.0.0.1:8080", "127.0.0.1:8080 ",
		"127.0.0.1:99999999999999999999999", "127.00.0.1:8080", "2130706433:8080",
	} {
		if _, err := ParseListenAddress(address); err == nil || strings.Contains(err.Error(), address) && address != "" {
			t.Fatalf("unsafe listen address was accepted or exposed unchecked content, case %q", address)
		}
	}
}

func TestAuthorityRequiresAnActualBoundLoopbackPort(t *testing.T) {
	var typedNil *net.TCPAddr
	for _, address := range []net.Addr{
		nil, typedNil, &net.UnixAddr{Name: "private", Net: "unix"},
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1")},
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: -1},
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 65536},
		&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 8080},
		&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 8080},
		&net.TCPAddr{IP: net.ParseIP("::1"), Port: 8080, Zone: "unchecked"},
		&net.TCPAddr{Port: 8080},
	} {
		if authority, err := NewAuthority(address); err == nil || authority.Valid() {
			t.Fatal("unbound or exposed authority was accepted")
		}
	}
	if (Authority{}).Valid() || (Authority{}).Accepts("127.0.0.1:8080") {
		t.Fatal("zero authority bypasses host protection")
	}
	for _, ip := range []string{"127.0.0.1", "::1"} {
		authority, err := NewAuthority(&net.TCPAddr{IP: net.ParseIP(ip), Port: 8080})
		if err != nil || !authority.Valid() {
			t.Fatal("bound local authority was rejected")
		}
		for _, host := range []string{
			"127.0.0.1:8080", "127.1.2.3:8080", "localhost:8080", "LOCALHOST:8080",
			"[::1]:8080", "[0:0:0:0:0:0:0:1]:8080", "[::ffff:127.0.0.1]:8080",
		} {
			if !authority.Accepts(host) {
				t.Fatalf("valid local Host %q was rejected", host)
			}
		}
		for _, host := range []string{
			"", "localhost", "127.0.0.1", "localhost:0", "localhost:8081",
			"localhost:+8080", "localhost:-8080", "localhost:65536", "localhost:http",
			"localhost:8080 ", " localhost:8080", "localhost:8080\n", "localhost.:8080",
			"example.invalid:8080", "localhost.example:8080", "user@localhost:8080",
			"127.0.0.1@evil.invalid:8080", "192.0.2.1:8080", "0.0.0.0:8080", "[::]:8080",
			"::1:8080", "[::1%loopback]:8080", "[localhost]:8080", "[127.0.0.1]:8080",
			"127.0.0.1:8080/path", "127.0.0.1:8080?query", "127.0.0.1:8080#fragment",
			"127.0.0.1:8080,localhost:8080", "http://127.0.0.1:8080", "127.00.0.1:8080",
		} {
			if authority.Accepts(host) {
				t.Fatalf("invalid Host %q was accepted", host)
			}
		}
	}
}
