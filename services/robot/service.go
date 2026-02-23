// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"context"
	"sync"
	"time"

	repo_model "code.gitea.io/gitea/models/repo"
)

// DefaultTTL is the default cache TTL (5 minutes)
const DefaultTTL = 5 * time.Minute

// IssueScore represents a single issue with its PageRank score
type IssueScore struct {
	IssueID int64   `json:"issue_id"`
	Score   float64 `json:"score"`
	Rank    int     `json:"rank"`
}

// TriageResponse represents the response from the triage endpoint
type TriageResponse struct {
	RepoID    int64        `json:"repo_id"`
	Owner     string       `json:"owner"`
	Repo      string       `json:"repo"`
	Issues    []IssueScore `json:"issues"`
	Cached    bool         `json:"cached"`
	Timestamp time.Time    `json:"timestamp"`
}

// Service provides robot API functionality
type Service struct {
	cache *Cache
}

var (
	serviceInstance *Service
	serviceOnce     sync.Once
)

// NewService creates or returns the singleton Service instance
func NewService() *Service {
	serviceOnce.Do(func() {
		serviceInstance = &Service{
			cache: NewCache(DefaultTTL),
		}
	})
	return serviceInstance
}

// NewServiceWithCache creates a new Service instance with a custom cache TTL
// This is useful for testing or when you need a non-singleton instance
func NewServiceWithCache(ttl time.Duration) *Service {
	return &Service{
		cache: NewCache(ttl),
	}
}

// shouldRecalculate determines if PageRank needs to be recalculated for a repository
// Returns true if recalculation is needed, false if cached result can be used
func (s *Service) shouldRecalculate(repoID int64) bool {
	// Check if cached result exists and is fresh
	_, found := s.cache.Get(repoID)
	// Return true if recalculation needed (cache miss or stale)
	return !found
}

// Triage performs issue triage using PageRank algorithm
// It uses cached results if available and fresh, otherwise recalculates
func (s *Service) Triage(ctx context.Context, repository *repo_model.Repository) (*TriageResponse, error) {
	// Check cache first
	if cached, found := s.cache.Get(repository.ID); found {
		cached.Cached = true
		return cached, nil
	}

	// Cache miss or stale - calculate PageRank
	// TODO: Implement actual PageRank calculation
	// For now, return empty response
	response := &TriageResponse{
		RepoID:    repository.ID,
		Owner:     repository.OwnerName,
		Repo:      repository.Name,
		Issues:    []IssueScore{},
		Cached:    false,
		Timestamp: time.Now(),
	}

	// Store result in cache
	s.cache.Set(repository.ID, response)

	return response, nil
}

// Cache provides thread-safe caching of TriageResponse with TTL
type Cache struct {
	mu      sync.RWMutex
	entries map[int64]*cacheEntry
	ttl     time.Duration
}

type cacheEntry struct {
	data      *TriageResponse
	timestamp time.Time
}

// NewCache creates a new cache with the specified TTL
func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Cache{
		entries: make(map[int64]*cacheEntry),
		ttl:     ttl,
	}
}

// Get retrieves a cached result if it exists and is fresh
func (c *Cache) Get(repoID int64) (*TriageResponse, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[repoID]
	if !exists {
		return nil, false
	}

	// Check if entry is still fresh
	if time.Since(entry.timestamp) > c.ttl {
		return nil, false
	}

	return entry.data, true
}

// Set stores a result in the cache
func (c *Cache) Set(repoID int64, data *TriageResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[repoID] = &cacheEntry{
		data:      data,
		timestamp: time.Now(),
	}
}

// Delete removes an entry from the cache
func (c *Cache) Delete(repoID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, repoID)
}

// Clear removes all entries from the cache
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[int64]*cacheEntry)
}

// Size returns the number of entries in the cache
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.entries)
}

// TTL returns the cache TTL
func (c *Cache) TTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.ttl
}

// Cleanup removes expired entries and returns count of removed items
func (c *Cache) Cleanup() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	now := time.Now()
	for repoID, entry := range c.entries {
		if now.Sub(entry.timestamp) > c.ttl {
			delete(c.entries, repoID)
			removed++
		}
	}
	return removed
}
