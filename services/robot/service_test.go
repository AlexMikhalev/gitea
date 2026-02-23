// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewService(t *testing.T) {
	svc1 := NewService()
	svc2 := NewService()

	// Should return the same singleton instance
	if svc1 == nil {
		t.Fatal("NewService returned nil")
	}
	if svc2 == nil {
		t.Fatal("NewService returned nil")
	}
	if svc1 != svc2 {
		t.Error("NewService should return singleton instance")
	}
}

func TestNewServiceWithCache(t *testing.T) {
	// Test with custom TTL
	ttl := 10 * time.Minute
	svc := NewServiceWithCache(ttl)

	if svc == nil {
		t.Fatal("NewServiceWithCache returned nil")
	}
	if svc.cache == nil {
		t.Fatal("Service cache not initialized")
	}
	if svc.cache.TTL() != ttl {
		t.Errorf("Expected TTL %v, got %v", ttl, svc.cache.TTL())
	}

	// Each call should return a new instance (not singleton)
	svc2 := NewServiceWithCache(ttl)
	if svc == svc2 {
		t.Error("NewServiceWithCache should return new instances")
	}
}

func TestNewServiceWithCache_ZeroTTL(t *testing.T) {
	// Zero TTL should default to DefaultTTL
	svc := NewServiceWithCache(0)
	if svc.cache.TTL() != DefaultTTL {
		t.Errorf("Expected TTL %v for zero input, got %v", DefaultTTL, svc.cache.TTL())
	}
}

func TestNewServiceWithCache_NegativeTTL(t *testing.T) {
	// Negative TTL should default to DefaultTTL
	svc := NewServiceWithCache(-1 * time.Second)
	if svc.cache.TTL() != DefaultTTL {
		t.Errorf("Expected TTL %v for negative input, got %v", DefaultTTL, svc.cache.TTL())
	}
}

func TestShouldRecalculate_CacheMiss(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)
	repoID := int64(1)

	// No cached entry - should recalculate
	if !svc.shouldRecalculate(repoID) {
		t.Error("Expected shouldRecalculate=true for cache miss")
	}
}

func TestShouldRecalculate_CacheHit(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)
	repoID := int64(1)

	// Pre-populate cache
	response := &TriageResponse{
		RepoID:    repoID,
		Owner:     "owner",
		Repo:      "repo",
		Issues:    []IssueScore{},
		Cached:    false,
		Timestamp: time.Now(),
	}
	svc.cache.Set(repoID, response)

	// Cached entry exists - should NOT recalculate
	if svc.shouldRecalculate(repoID) {
		t.Error("Expected shouldRecalculate=false for fresh cache entry")
	}
}

func TestShouldRecalculate_CacheExpired(t *testing.T) {
	// Use short TTL for testing
	svc := NewServiceWithCache(50 * time.Millisecond)
	repoID := int64(1)

	// Pre-populate cache
	response := &TriageResponse{
		RepoID:    repoID,
		Owner:     "owner",
		Repo:      "repo",
		Issues:    []IssueScore{},
		Cached:    false,
		Timestamp: time.Now(),
	}
	svc.cache.Set(repoID, response)

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	// Cached entry expired - should recalculate
	if !svc.shouldRecalculate(repoID) {
		t.Error("Expected shouldRecalculate=true for expired cache entry")
	}
}

func TestTriage_CacheHit_NoRecalculation(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)
	repo := &Repository{
		ID:        1,
		OwnerName: "owner",
		Name:      "repo",
	}

	// First call to populate cache
	ctx := sync.Mutex{}
	_ = ctx // Use ctx to avoid unused import
	resp1, err := svc.Triage(nil, repo)
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}
	if resp1 == nil {
		t.Fatal("First response is nil")
	}
	if resp1.Cached {
		t.Error("First response should not be cached")
	}

	// Second call should hit cache
	resp2, err := svc.Triage(nil, repo)
	if err != nil {
		t.Fatalf("Second call failed: %v", err)
	}
	if resp2 == nil {
		t.Fatal("Second response is nil")
	}
	if !resp2.Cached {
		t.Error("Second response should be cached")
	}

	// Should be the same data (timestamp should match)
	if resp1.Timestamp != resp2.Timestamp {
		t.Error("Cached responses should have same timestamp")
	}
}

func TestTriage_CacheMiss_Recalculation(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)
	repo := &Repository{
		ID:        1,
		OwnerName: "owner",
		Name:      "repo",
	}

	resp, err := svc.Triage(nil, repo)
	if err != nil {
		t.Fatalf("Triage failed: %v", err)
	}
	if resp == nil {
		t.Fatal("Response is nil")
	}
	if resp.RepoID != repo.ID {
		t.Errorf("Expected RepoID %d, got %d", repo.ID, resp.RepoID)
	}
	if resp.Owner != repo.OwnerName {
		t.Errorf("Expected Owner %s, got %s", repo.OwnerName, resp.Owner)
	}
	if resp.Repo != repo.Name {
		t.Errorf("Expected Repo %s, got %s", repo.Name, resp.Repo)
	}
	if resp.Cached {
		t.Error("First response should not be cached")
	}
	if resp.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestTriage_RateLimiting_SequentialCalls(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)
	repo := &Repository{
		ID:        1,
		OwnerName: "owner",
		Name:      "repo",
	}

	// First call - should calculate
	resp1, err := svc.Triage(nil, repo)
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}
	if resp1.Cached {
		t.Error("First call should not be cached")
	}

	// Multiple sequential calls - should use cache
	for i := 0; i < 5; i++ {
		resp, err := svc.Triage(nil, repo)
		if err != nil {
			t.Fatalf("Call %d failed: %v", i+2, err)
		}
		if !resp.Cached {
			t.Errorf("Call %d should be cached", i+2)
		}
		if resp.Timestamp != resp1.Timestamp {
			t.Errorf("Call %d should have same timestamp as first response", i+2)
		}
	}

	// Cache should have exactly 1 entry
	if svc.cache.Size() != 1 {
		t.Errorf("Expected cache size 1, got %d", svc.cache.Size())
	}
}

func TestTriage_RateLimiting_DifferentRepos(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)

	// Call for different repos
	repos := []*Repository{
		{ID: 1, OwnerName: "owner1", Name: "repo1"},
		{ID: 2, OwnerName: "owner2", Name: "repo2"},
		{ID: 3, OwnerName: "owner3", Name: "repo3"},
	}

	for _, repo := range repos {
		resp, err := svc.Triage(nil, repo)
		if err != nil {
			t.Fatalf("Call for repo %d failed: %v", repo.ID, err)
		}
		if resp.Cached {
			t.Errorf("First call for repo %d should not be cached", repo.ID)
		}
		if resp.RepoID != repo.ID {
			t.Errorf("Expected RepoID %d, got %d", repo.ID, resp.RepoID)
		}
	}

	// Cache should have 3 entries
	if svc.cache.Size() != 3 {
		t.Errorf("Expected cache size 3, got %d", svc.cache.Size())
	}

	// Second call for each repo should hit cache
	for _, repo := range repos {
		resp, err := svc.Triage(nil, repo)
		if err != nil {
			t.Fatalf("Second call for repo %d failed: %v", repo.ID, err)
		}
		if !resp.Cached {
			t.Errorf("Second call for repo %d should be cached", repo.ID)
		}
	}

	// Cache should still have 3 entries
	if svc.cache.Size() != 3 {
		t.Errorf("Expected cache size 3 after second round, got %d", svc.cache.Size())
	}
}

func TestTriage_CacheExpiration_Recalculates(t *testing.T) {
	svc := NewServiceWithCache(50 * time.Millisecond)
	repo := &Repository{
		ID:        1,
		OwnerName: "owner",
		Name:      "repo",
	}

	// First call - should calculate
	resp1, err := svc.Triage(nil, repo)
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}
	if resp1.Cached {
		t.Error("First call should not be cached")
	}

	// Wait for cache to expire
	time.Sleep(100 * time.Millisecond)

	// Second call - should recalculate due to expiration
	resp2, err := svc.Triage(nil, repo)
	if err != nil {
		t.Fatalf("Second call failed: %v", err)
	}
	if resp2.Cached {
		t.Error("Response after expiration should not be cached")
	}

	// Timestamps should be different
	if resp1.Timestamp == resp2.Timestamp {
		t.Error("Timestamps should be different after recalculation")
	}
}

func TestTriage_ConcurrentAccess(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)
	repo := &Repository{
		ID:        1,
		OwnerName: "owner",
		Name:      "repo",
	}

	var wg sync.WaitGroup
	numGoroutines := 50
	wg.Add(numGoroutines)

	// Run concurrent Triage calls
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := svc.Triage(nil, repo)
			if err != nil {
				t.Errorf("Triage failed: %v", err)
			}
		}()
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(10 * time.Second):
		t.Fatal("Concurrent Triage calls timed out")
	}

	// Cache should have exactly 1 entry
	if svc.cache.Size() != 1 {
		t.Errorf("Expected cache size 1, got %d", svc.cache.Size())
	}
}

func TestServiceCacheIntegration(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)

	// Verify cache is accessible
	if svc.cache.Size() != 0 {
		t.Errorf("Expected empty cache, got size %d", svc.cache.Size())
	}

	// Add entries directly to cache
	repoID := int64(42)
	response := &TriageResponse{
		RepoID:    repoID,
		Owner:     "test",
		Repo:      "repo",
		Issues:    []IssueScore{{IssueID: 1, Score: 0.5, Rank: 1}},
		Cached:    false,
		Timestamp: time.Now(),
	}

	svc.cache.Set(repoID, response)
	if svc.cache.Size() != 1 {
		t.Errorf("Expected cache size 1, got %d", svc.cache.Size())
	}

	// Verify entry can be retrieved
	cached, found := svc.cache.Get(repoID)
	if !found {
		t.Fatal("Expected cache hit")
	}
	if cached.RepoID != response.RepoID {
		t.Errorf("Expected RepoID %d, got %d", response.RepoID, cached.RepoID)
	}

	// Delete entry
	svc.cache.Delete(repoID)
	if svc.cache.Size() != 0 {
		t.Errorf("Expected empty cache after delete, got size %d", svc.cache.Size())
	}
}

func TestServiceWithCache_CountsCalculations(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)

	var calculationCount int32
	numRepos := 5
	numCallsPerRepo := 10

	// Simulate multiple calls for multiple repos
	for i := 0; i < numCallsPerRepo; i++ {
		for j := 0; j < numRepos; j++ {
			repoID := int64(j)

			// Check if this will trigger a calculation
			if i == 0 {
				// First round - always calculate
				atomic.AddInt32(&calculationCount, 1)
				response := &TriageResponse{
					RepoID:    repoID,
					Owner:     "owner",
					Repo:      "repo",
					Issues:    []IssueScore{{IssueID: repoID, Score: float64(repoID) * 0.1, Rank: int(repoID)}},
					Cached:    false,
					Timestamp: time.Now(),
				}
				svc.cache.Set(repoID, response)
			}
		}
	}

	// Should have exactly numRepos calculations (one per repo)
	if atomic.LoadInt32(&calculationCount) != int32(numRepos) {
		t.Errorf("Expected %d calculations (one per repo), got %d", numRepos, calculationCount)
	}

	// Cache should have numRepos entries
	if svc.cache.Size() != numRepos {
		t.Errorf("Expected cache size %d, got %d", numRepos, svc.cache.Size())
	}
}

func TestCacheHitMissBehavior(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)

	// Test cache miss
	if !svc.shouldRecalculate(1) {
		t.Error("Expected shouldRecalculate=true for non-existent entry")
	}

	// Add entry
	svc.cache.Set(1, &TriageResponse{
		RepoID:    1,
		Owner:     "owner",
		Repo:      "repo",
		Issues:    []IssueScore{},
		Cached:    false,
		Timestamp: time.Now(),
	})

	// Test cache hit
	if svc.shouldRecalculate(1) {
		t.Error("Expected shouldRecalculate=false for fresh entry")
	}

	// Verify cache hit returns correct data
	cached, found := svc.cache.Get(1)
	if !found {
		t.Fatal("Expected cache hit")
	}
	if cached.RepoID != 1 {
		t.Errorf("Expected RepoID 1, got %d", cached.RepoID)
	}
}

func TestCacheHitMiss_MultipleOperations(t *testing.T) {
	svc := NewServiceWithCache(5 * time.Minute)

	// Initial state - all should be cache misses
	for i := int64(1); i <= 5; i++ {
		if !svc.shouldRecalculate(i) {
			t.Errorf("Expected cache miss for repo %d", i)
		}
	}

	// Populate cache for repos 1-3
	for i := int64(1); i <= 3; i++ {
		svc.cache.Set(i, &TriageResponse{
			RepoID:    i,
			Owner:     "owner",
			Repo:      "repo",
			Issues:    []IssueScore{},
			Cached:    false,
			Timestamp: time.Now(),
		})
	}

	// Now repos 1-3 should be hits, 4-5 should be misses
	for i := int64(1); i <= 3; i++ {
		if svc.shouldRecalculate(i) {
			t.Errorf("Expected cache hit for repo %d", i)
		}
	}
	for i := int64(4); i <= 5; i++ {
		if !svc.shouldRecalculate(i) {
			t.Errorf("Expected cache miss for repo %d", i)
		}
	}
}

func BenchmarkShouldRecalculate_CacheHit(b *testing.B) {
	svc := NewServiceWithCache(5 * time.Minute)
	svc.cache.Set(1, &TriageResponse{RepoID: 1, Timestamp: time.Now()})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.shouldRecalculate(1)
	}
}

func BenchmarkShouldRecalculate_CacheMiss(b *testing.B) {
	svc := NewServiceWithCache(5 * time.Minute)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.shouldRecalculate(int64(i))
	}
}
