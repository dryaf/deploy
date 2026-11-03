package interaction

import "testing"

func TestExplicitGrammar(t *testing.T) {
	p, err := Parse(`vuln/pi accept-risk 90d subject=frontend reason="Build-only dependency; absent from runtime"`)
	if err != nil || p.Worker != "vuln/pi" || p.Arguments["duration"] != "90d" || p.Arguments["reason"] != "Build-only dependency; absent from runtime" {
		t.Fatalf("parse: %+v %v", p, err)
	}
	for _, text := range []string{"rescan", "vuln/pi status\nbackup/storage retry", `vuln/pi status x=1 x=2`, `vuln/pi status reason="unterminated`, "vuln/pi status\x00"} {
		if _, err := Parse(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	d := Descriptor{Arguments: map[string]Argument{"duration": {Kind: "duration", Required: true, Max: 90 * 86400}}}
	for _, duration := range []string{"91d", "-1h", "0s", "99999999999999d"} {
		if ValidateArguments(d, map[string]string{"duration": duration}) == nil {
			t.Fatal(duration)
		}
	}
}
func TestScopeSubset(t *testing.T) {
	a := Scope{Subjects: []string{"a"}}
	if a.Allows(Scope{Subjects: []string{"b"}}) || a.Allows(Scope{WholeWorker: true}) || a.Allows(Scope{}) {
		t.Fatal("scope widened")
	}
	if !a.Allows(a) || !(Scope{WholeWorker: true}).Allows(a) {
		t.Fatal("valid scope denied")
	}
}

func TestTargetedArgumentsCannotSelectWholeSubject(t *testing.T) {
	d := Descriptor{TargetRequired: true, Arguments: map[string]Argument{"subject": {Kind: "token", Required: true, Max: 128}, "target": {Kind: "token", Required: true, Max: 128}}}
	if ValidateArguments(d, map[string]string{"subject": "frontend"}) == nil {
		t.Fatal("missing finding or incident target accepted")
	}
	if err := ValidateArguments(d, map[string]string{"subject": "frontend", "target": "finding-one"}); err != nil {
		t.Fatal(err)
	}
}
