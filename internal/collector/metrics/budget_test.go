package metrics

import "testing"

func fam(name string, n int) ParsedFamily {
	f := ParsedFamily{Name: name}
	for i := 0; i < n; i++ {
		f.Metrics = append(f.Metrics, ParsedMetric{Value: float64(i)})
	}
	return f
}

func TestBudgetZeroMeansNoCeiling(t *testing.T) {
	in := []ParsedFamily{fam("b", 500), fam("a", 500)}

	kept, dropped := applySampleBudget(in, 0)

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if len(kept) != 2 {
		t.Errorf("kept %d families, want 2", len(kept))
	}
}

func TestBudgetKeepsWholeFamiliesInNameOrder(t *testing.T) {
	in := []ParsedFamily{fam("c", 40), fam("a", 30), fam("b", 40)}

	kept, dropped := applySampleBudget(in, 75)

	// Whole families, never a partial one: half a histogram is not a smaller
	// histogram, it is a wrong one. Name order makes the choice deterministic,
	// so two consecutive scrapes of an over-budget target keep the SAME
	// families and the resulting series do not flap in and out of existence.
	if len(kept) != 2 || kept[0].Name != "a" || kept[1].Name != "b" {
		t.Fatalf("kept = %v, want [a b]", names(kept))
	}
	if dropped != 40 {
		t.Errorf("dropped = %d, want 40 (family c)", dropped)
	}
}

func TestBudgetSmallerThanTheFirstFamilyKeepsNothing(t *testing.T) {
	in := []ParsedFamily{fam("a", 100)}

	kept, dropped := applySampleBudget(in, 10)

	if len(kept) != 0 {
		t.Fatalf("kept = %v, want none", names(kept))
	}
	if dropped != 100 {
		t.Errorf("dropped = %d, want 100", dropped)
	}
}

func TestBudgetDoesNotMutateTheInput(t *testing.T) {
	in := []ParsedFamily{fam("b", 40), fam("a", 40)}

	applySampleBudget(in, 40)

	if in[0].Name != "b" || in[1].Name != "a" {
		t.Fatalf("input was reordered: %v", names(in))
	}
}

func names(list []ParsedFamily) []string {
	out := make([]string, len(list))
	for i, f := range list {
		out[i] = f.Name
	}
	return out
}

func TestABudgetedDropIsCountedAgainstTheRightKind(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 1)

	families := []ParsedFamily{fam("container_cpu_usage_seconds_total", 100)}
	kept, dropped := applySampleBudget(families, 10)
	if dropped > 0 {
		h.RecordCardinalityDrop("cadvisor", dropped)
	}
	h.RecordSuccess("cadvisor", "cadvisor/node-1", countSamples(kept))

	got := byKind(h.Snapshot())["cadvisor"]
	if got.DroppedCardinality != 100 {
		t.Errorf("DroppedCardinality = %d, want 100", got.DroppedCardinality)
	}
	// The reported sample count is what was PUBLISHED, not what was scraped.
	// Reporting the scraped figure next to a drop counter would make the two
	// numbers disagree with the data that actually arrived.
	if got.SamplesLastScrape != 0 {
		t.Errorf("SamplesLastScrape = %d, want 0", got.SamplesLastScrape)
	}
	// A budgeted drop is not a failure: the target answered.
	if got.State != StateOK {
		t.Errorf("State = %q, want %q", got.State, StateOK)
	}
}
