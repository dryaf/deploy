package install

import (
	"context"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestDeliveryAdmissionTypedRetryPinsQualification(t *testing.T) {
	spec, i, services, _, before, record := deliveryFixture(t)
	ctx := context.Background()
	r, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(before))
	if err != nil || !ready || r.QualificationHash != protocol.Hash(record) {
		t.Fatal(r, ready, err)
	}
	if _, complete, err := i.inspectDeliveryAdmission(ctx, r); err != nil || complete {
		t.Fatal(complete, err)
	}
	stop := errors.New("lost activation reply")
	i.fault = func(at string) error {
		if at == "published" {
			return stop
		}
		return nil
	}
	if _, err = i.prepareDelivery(ctx, "activate-delivery", r.Root, r.ManifestHash, r.QualificationHash, false); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if _, complete, err := i.inspectDeliveryAdmission(ctx, r); err != nil || complete {
		t.Fatal(complete, err)
	}
	changed := r
	changed.QualificationHash = protocol.Hash("different report")
	if _, _, err = i.inspectDeliveryAdmission(ctx, changed); err == nil {
		t.Fatal("adopted different qualification")
	}
	i.fault = nil
	after, err := i.prepareDelivery(ctx, "activate-delivery", r.Root, r.ManifestHash, r.QualificationHash, false)
	if err != nil {
		t.Fatal(err)
	}
	starts, stops := services.starts, services.stops
	got, complete, err := i.inspectDeliveryAdmission(ctx, r)
	if err != nil || !complete || protocol.Hash(got) != protocol.Hash(after) || services.starts != starts || services.stops != stops {
		t.Fatal("completion inspection changed service", complete, err)
	}
}

func TestDeliveryAdmissionReviewDoesNotInventQualification(t *testing.T) {
	spec, _, _, report := qualificationFixture(t)
	_, ready, err := ReviewDeliveryAdmission(context.Background(), spec.Root, protocol.Hash(report.Binding.Manifest))
	if err != nil || ready {
		t.Fatal(ready, err)
	}
	if _, err = qualifyFixture(t, spec.Root, report); err != nil {
		t.Fatal(err)
	}
	_, ready, err = ReviewDeliveryAdmission(context.Background(), spec.Root, protocol.Hash(report.Binding.Manifest))
	if err != nil || ready {
		t.Fatal("pending checks became admitted", ready, err)
	}
}
