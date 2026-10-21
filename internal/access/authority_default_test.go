package access

import (
	"net"
	"testing"
)

func TestAuthorityHonorsTheDefaultHTTPPort(t *testing.T) {
	authority, err := NewAuthority(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "[::ffff:127.0.0.1]", "localhost:80"} {
		if !authority.Accepts(host) {
			t.Errorf("valid default-port HTTP authority %q was rejected", host)
		}
	}
	for _, host := range []string{"", "::1", "localhost:", "[localhost]", "[127.0.0.1]", "localhost:81", "attacker.invalid", "[::1%zone]"} {
		if authority.Accepts(host) {
			t.Errorf("invalid default-port authority %q was accepted", host)
		}
	}
	other, err := NewAuthority(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if other.Accepts(host) {
			t.Fatal("an omitted port matched a nondefault listener")
		}
	}
}
