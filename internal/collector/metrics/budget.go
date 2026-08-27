package metrics

import "sort"

// applySampleBudget caps one scrape's samples.
//
// The unit is SAMPLES, not families: a family's cost is the number of series
// it carries, and cAdvisor's expensive families are expensive because a
// cluster has many containers, not because there are many families.
//
// Families are kept whole and in name order. Both halves matter:
//
//   - Whole, because half a histogram is a wrong histogram, not a smaller one.
//   - In name order, because the choice must be the same on every scrape. A
//     budget that kept whatever arrived first would admit a different subset
//     each time, and every series in the losing subset would appear and
//     disappear -- which reads downstream as data loss rather than as a cap.
//
// budget <= 0 means no ceiling.
func applySampleBudget(families []ParsedFamily, budget int) (kept []ParsedFamily, dropped int64) {
	if budget <= 0 {
		return families, 0
	}

	// A copy: the caller's slice is the scrape result and reordering it in
	// place would reorder what the caller then publishes.
	ordered := make([]ParsedFamily, len(families))
	copy(ordered, families)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	spent := 0
	kept = make([]ParsedFamily, 0, len(ordered))
	for _, f := range ordered {
		cost := len(f.Metrics)
		if spent+cost > budget {
			dropped += int64(cost)
			continue
		}
		spent += cost
		kept = append(kept, f)
	}
	return kept, dropped
}
