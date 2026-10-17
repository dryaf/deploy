package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stageBinaryFixture(f *transactionFixture, id string, artifacts map[string]string, syncEnv bool) string {
	f.t.Helper()
	original := f.stage(id, artifacts, syncEnv)
	destination := filepath.Join(f.target, ".deploy/binary/staging", id)
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(original, destination); err != nil {
		f.t.Fatal(err)
	}
	var request imageReleaseRequest
	if err := json.Unmarshal([]byte(f.read(filepath.Join(destination, "request.json"))), &request); err != nil {
		f.t.Fatal(err)
	}
	request.Reference = "localhost/pdf:latest"
	request.Stop = true
	body, err := json.Marshal(binaryReleaseRequest{imageReleaseRequest: request, Dockerfile: "Dockerfile.vps"})
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(destination, "request.json"), string(body), 0600)
	f.write(filepath.Join(destination, "candidate.container"), "[Container]\nImage=localhost/pdf:latest\nHealthCmd=true\n[Service]\nRestart=on-failure\n[Install]\nWantedBy=default.target\n", 0600)
	f.write(f.script, binaryTransactionScript, 0700)
	return destination
}

func TestBinaryTransactionFirstInstallAndRollback(t *testing.T) {
	for _, point := range []string{"fail-build", "fail-health", "fail-install", "fail-reload", "fail-restart", "fail-target-sync", "kill-after-unit", "kill-before-commit"} {
		t.Run(point, func(t *testing.T) {
			f := newTransactionFixture(t)
			f.success(stageBinaryFixture(f, "A", map[string]string{"server": "old binary", "Dockerfile.vps": "old dockerfile", "config/value": "old config"}, true))
			if strings.Contains(f.read(filepath.Join(f.root, "calls")), "systemctl stop") {
				t.Fatal("first install stopped a nonexistent service")
			}
			oldUnit := f.read(f.unit())
			state := filepath.Join(f.target, ".deploy/binary")
			oldCurrent := f.read(filepath.Join(state, "current.json"))
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark(point)
			stage := stageBinaryFixture(f, "B", map[string]string{"server": "new binary", "Dockerfile.vps": "broken dockerfile", "config/value": "new config", "config/new": "new"}, true)
			out, err := f.run("release", stage)
			if err == nil {
				t.Fatal("failure was ignored", out)
			}
			if strings.HasPrefix(point, "kill-") {
				if out, err := f.run("action", stageBinaryFixture(f, "recover", nil, false), "start"); err != nil {
					t.Fatal(out, err)
				}
			}
			if f.read(f.unit()) != oldUnit || f.read(filepath.Join(state, "current.json")) != oldCurrent {
				t.Fatal("unit or current record not restored", out)
			}
			for name, want := range map[string]string{"server": "old binary", "Dockerfile.vps": "old dockerfile", "config/value": "old config", ".env": "SECRET=A"} {
				if got := f.read(filepath.Join(f.target, name)); got != want {
					t.Fatalf("%s: %s", name, got)
				}
			}
			if f.read(filepath.Join(f.root, "running-image")) != imageA {
				t.Fatal("old image not restored", out)
			}
			f.assertAbsent(filepath.Join(f.target, "config/new"))
			f.assertAbsent(filepath.Join(state, "pending.json"))
		})
	}
}

func TestBinaryLegacyQuadletAndInactiveStateRollback(t *testing.T) {
	f := newTransactionFixture(t)
	legacy := "[Container]\nImage=localhost/pdf:legacy\nHealthCmd=true\n[Install]\nWantedBy=default.target\n"
	f.write(f.unit(), legacy, 0600)
	f.write(filepath.Join(f.root, "images", imageA), "", 0600)
	f.write(filepath.Join(f.root, "tags", "localhost_pdf:legacy"), imageA, 0600)
	f.write(filepath.Join(f.target, "server"), "legacy", 0755)
	f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
	f.mark("fail-health")
	out, err := f.run("release", stageBinaryFixture(f, "A", map[string]string{"server": "new"}, false))
	if err == nil || strings.Contains(out, "ROLLBACK FAILED") {
		t.Fatal(out, err)
	}
	if f.read(f.unit()) != legacy || f.read(filepath.Join(f.target, "server")) != "legacy" {
		t.Fatal("legacy deployment not restored", out)
	}
	f.assertAbsent(filepath.Join(f.root, "active"))
}

func TestBinaryUIDWithoutOwnershipPaths(t *testing.T) {
	f := newTransactionFixture(t)
	stage := stageBinaryFixture(f, "A", map[string]string{"server": "binary"}, false)
	var request binaryReleaseRequest
	if err := json.Unmarshal([]byte(f.read(filepath.Join(stage, "request.json"))), &request); err != nil {
		t.Fatal(err)
	}
	request.UID = 65532
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(stage, "request.json"), string(body), 0600)
	f.success(stage)
	if strings.Contains(f.read(filepath.Join(f.root, "calls")), "podman unshare") {
		t.Fatal("changed ownership without configured paths")
	}
}

func TestBinaryBuildPinsItsOwnImageDespiteTagMutation(t *testing.T) {
	f := newTransactionFixture(t)
	f.mark("mutate-build-tag")
	f.success(stageBinaryFixture(f, "A", map[string]string{"server": "binary"}, false))
	if f.read(filepath.Join(f.root, "running-image")) != imageA {
		t.Fatal("deployed another build's image")
	}
}
