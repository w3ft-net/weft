package store

import "testing"

func TestValidateNamespaceName(t *testing.T) {
	ok := []string{"acme", "production", "prod-east", "my_ns", "Acme", "ns2", "a.b", "日本"}
	bad := []string{"", "a,b", "a b", "a\tb", "a/b", "a+tokens", "+x", "a*", "a?", "a[1]", "-flag", "--help", "a\nb", "a\x00b"}
	for _, n := range ok {
		if err := ValidateNamespaceName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateNamespaceName(n); err == nil {
			t.Errorf("%q should be refused", n)
		}
	}
}
