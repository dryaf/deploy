package signalaccount

import "testing"

func TestNativeAccountIdentitySelection(t *testing.T) {
	const account = "11111111-1111-4111-8111-111111111111"
	valid := `[{"number":"+10000000000","aci":"` + account + `"}]`
	for _, selector := range []string{"+10000000000", account} {
		actual, err := ResolveNativeAccountIdentity([]byte(valid), selector)
		if err != nil || actual != account {
			t.Fatal(actual, err)
		}
	}
	for _, raw := range []string{`[]`, `null`, `{}`, `[{"number":"+10000000000","aci":"bad"}]`, `[{"number":"+19999999999","aci":"` + account + `"}]`, valid + ` {}`, `[{"number":"+10000000000","aci":"` + account + `"},{"number":"+10000000000","aci":"` + account + `"}]`} {
		if _, err := ResolveNativeAccountIdentity([]byte(raw), "+10000000000"); err == nil {
			t.Fatal("ambiguous or invalid account accepted")
		}
	}
}
