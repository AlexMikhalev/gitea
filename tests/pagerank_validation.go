package main

import (
	"fmt"
)

// Simple in-memory PageRank test

type Dependency struct {
	IssueID   int
	DependsOn int
	DepType   string
}

func main() {
	fmt.Println("=== Gitea Robot PageRank Test ===\n")

	// Test case: Chain 1 -> 2 -> 3
	// Issue 1 blocks Issue 2
	// Issue 2 blocks Issue 3
	deps := []Dependency{
		{IssueID: 2, DependsOn: 1, DepType: "blocks"},
		{IssueID: 3, DependsOn: 2, DepType: "blocks"},
	}

	fmt.Println("Dependency graph:")
	fmt.Println("  Issue 1 (root)")
	fmt.Println("    └── blocks Issue 2")
	fmt.Println("          └── blocks Issue 3")
	fmt.Println()

	// Calculate PageRank
	ranks := calculatePageRank(deps, 0.85, 100)

	fmt.Println("PageRank scores after 100 iterations:")
	for i := 1; i <= 3; i++ {
		fmt.Printf("  Issue %d: %.6f\n", i, ranks[i])
	}
	fmt.Println()

	// Validate ordering
	if ranks[3] > ranks[2] && ranks[2] > ranks[1] {
		fmt.Println("✓ PASS: PageRank ordering correct (3 > 2 > 1)")
		fmt.Println("  Issue 3 has highest rank (most downstream)")
		fmt.Println("  Issue 1 has lowest rank (root, no incoming)")
	} else {
		fmt.Println("✗ FAIL: PageRank ordering incorrect!")
		fmt.Printf("  Expected: 3 > 2 > 1, Got: %.6f > %.6f > %.6f\n",
			ranks[3], ranks[2], ranks[1])
	}

	// Test 2: Star pattern
	fmt.Println("\n--- Test 2: Star Pattern ---")
	fmt.Println("Issue 1 blocks Issues 2, 3, 4")

	deps2 := []Dependency{
		{IssueID: 2, DependsOn: 1, DepType: "blocks"},
		{IssueID: 3, DependsOn: 1, DepType: "blocks"},
		{IssueID: 4, DependsOn: 1, DepType: "blocks"},
	}

	ranks2 := calculatePageRank(deps2, 0.85, 100)

	fmt.Println("PageRank scores:")
	for i := 1; i <= 4; i++ {
		fmt.Printf("  Issue %d: %.6f\n", i, ranks2[i])
	}

	// Children should have equal rank
	if approxEqual(ranks2[2], ranks2[3]) && approxEqual(ranks2[3], ranks2[4]) {
		fmt.Println("✓ PASS: Children have equal PageRank")
	} else {
		fmt.Println("✗ FAIL: Children should have equal rank")
	}

	if ranks2[2] > ranks2[1] {
		fmt.Println("✓ PASS: Children have higher rank than parent")
	}

	// Test 3: Diamond pattern
	fmt.Println("\n--- Test 3: Diamond Pattern ---")
	fmt.Println("1 -> 2, 1 -> 3, 2 -> 4, 3 -> 4")

	deps3 := []Dependency{
		{IssueID: 2, DependsOn: 1, DepType: "blocks"},
		{IssueID: 3, DependsOn: 1, DepType: "blocks"},
		{IssueID: 4, DependsOn: 2, DepType: "blocks"},
		{IssueID: 4, DependsOn: 3, DepType: "blocks"},
	}

	ranks3 := calculatePageRank(deps3, 0.85, 100)

	fmt.Println("PageRank scores:")
	for i := 1; i <= 4; i++ {
		fmt.Printf("  Issue %d: %.6f\n", i, ranks3[i])
	}

	if ranks3[4] > ranks3[2] && ranks3[4] > ranks3[3] {
		fmt.Println("✓ PASS: Issue 4 (leaf) has highest PageRank")
	}

	fmt.Println("\n=== All Tests Complete ===")
}

func calculatePageRank(deps []Dependency, damping float64, iterations int) map[int]float64 {
	// Collect all issue IDs
	issueSet := make(map[int]bool)
	for _, d := range deps {
		issueSet[d.IssueID] = true
		issueSet[d.DependsOn] = true
	}

	var issues []int
	for id := range issueSet {
		issues = append(issues, id)
	}

	// Initialize ranks
	ranks := make(map[int]float64)
	n := len(issues)
	for _, id := range issues {
		ranks[id] = 1.0 / float64(n)
	}

	// Build reverse adjacency: for each issue, who does it block?
	// If A blocks B, then B depends on A
	// We want B to have higher rank (downstream)
	blockedBy := make(map[int][]int) // issue -> list of issues that block it
	blocks := make(map[int][]int)    // issue -> list of issues it blocks
	for _, d := range deps {
		if d.DepType == "blocks" {
			blockedBy[d.IssueID] = append(blockedBy[d.IssueID], d.DependsOn)
			blocks[d.DependsOn] = append(blocks[d.DependsOn], d.IssueID)
		}
	}

	// Power iteration
	// For dependency tracking: downstream issues (blocked) should have higher rank
	// Rank flows from blockers to blocked issues
	for i := 0; i < iterations; i++ {
		newRanks := make(map[int]float64)
		for _, id := range issues {
			newRank := (1.0 - damping) / float64(n)

			// Sum contributions from blockers (upstream)
			// If A blocks B, then B's rank gets contribution from A
			for _, blockerID := range blockedBy[id] {
				outDegree := len(blocks[blockerID])
				if outDegree == 0 {
					outDegree = 1
				}
				newRank += damping * ranks[blockerID] / float64(outDegree)
			}

			newRanks[id] = newRank
		}
		ranks = newRanks
	}

	return ranks
}

func approxEqual(a, b float64) bool {
	return abs(a-b) < 0.0001
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}