package api

import (
	"math"
	"testing"
)

func TestComputeTCO(t *testing.T) {
	// 100 GiB stored, 50 GB egress/month.
	resp := computeTCO(100*gib, 50)

	if math.Abs(resp.StorageGb-100) > 0.001 {
		t.Fatalf("storageGb = %f, want 100", resp.StorageGb)
	}
	if len(resp.Providers) != len(tcoPricing) {
		t.Fatalf("got %d providers, want %d", len(resp.Providers), len(tcoPricing))
	}

	byName := map[string]tcoProvider{}
	for _, p := range resp.Providers {
		byName[p.Name] = p
	}

	// AWS: 100*0.023 storage + 50*0.09 egress = 2.30 + 4.50 = 6.80
	aws := byName["AWS S3 Standard"]
	if math.Abs(aws.StorageCost-2.30) > 0.001 || math.Abs(aws.EgressCost-4.50) > 0.001 || math.Abs(aws.MonthlyTotal-6.80) > 0.001 {
		t.Fatalf("AWS costs wrong: %+v", aws)
	}

	// R2: free egress → egress cost is 0 regardless of egressGb.
	r2 := byName["Cloudflare R2"]
	if r2.EgressCost != 0 {
		t.Fatalf("R2 egress should be 0, got %f", r2.EgressCost)
	}
	if math.Abs(r2.MonthlyTotal-1.50) > 0.001 { // 100*0.015
		t.Fatalf("R2 total = %f, want 1.50", r2.MonthlyTotal)
	}
}

func TestComputeTCOZeroData(t *testing.T) {
	resp := computeTCO(0, 0)
	for _, p := range resp.Providers {
		if p.MonthlyTotal != 0 {
			t.Fatalf("%s should be $0 for empty store, got %f", p.Name, p.MonthlyTotal)
		}
	}
}
