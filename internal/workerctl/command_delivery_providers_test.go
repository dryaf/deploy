package workerctl

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandProviderRetriesPinOldAndSelectedPrivateFiles(t *testing.T) {
	for _, kind := range []string{"command-provider", "command-destination"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newCommandPolicyFixture(t, "mutation", kind)
			f.policyUpdate(t, kind, 900)
			f.boundary = "delivery-provider-intent"
			if _, _, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider); err == nil {
				t.Fatal("missed retained intent")
			}
			j := f.journal(t)
			v, err := readDeliveryProviderIntent(f.d, j)
			testutil.Must(t, err)
			for _, raw := range [][]byte{v.BeforeRuntime, v.AfterRuntime} {
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				for _, path := range []string{input.Providers["telegram"].TokenFile, input.Providers["telegram"].ChatFile} {
					original, err := os.ReadFile(path)
					testutil.Must(t, err)
					testutil.Must(t, os.WriteFile(path, []byte("changed-unreviewed-private-file"), 0600))
					tree := planTree(t, f.paths.Control)
					effects := f.pauses + f.rotations
					if _, _, err = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider); err == nil {
						t.Fatal("changed credential accepted")
					}
					if f.pauses+f.rotations != effects || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
						t.Fatal("credential refusal changed intent or performed effects")
					}
					testutil.Must(t, os.WriteFile(path, original, 0600))
				}
			}
			f.boundary = ""
			_, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
			testutil.Must(t, err)
			if !handled {
				t.Fatal("retained rotation not resumed")
			}
		})
	}
}
