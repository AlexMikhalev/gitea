// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadAgentFrom(t *testing.T) {
	defer func(cfgMiB, bytes int64) {
		agentConfig.MaxRequestBodySize, Agent.MaxRequestBodySize = cfgMiB, bytes
	}(agentConfig.MaxRequestBodySize, Agent.MaxRequestBodySize)

	// loadAgentFrom runs more than once per process - cmd/main.go loads the common settings at
	// startup, and the installer calls LoadCommonSettings() again after the form is submitted,
	// in the same process that then goes on to serve. mustMapSetting leaves the field untouched
	// when the [agent] section is absent, which is the shipping default (app.example.ini has the
	// whole section commented out), so converting MiB to bytes in place used to square the
	// multiplier: the second load turned the 32 MiB cap into 32 TiB and the operator-chosen
	// ceiling that services/auth/nostr.go passes to VerifyPayload was silently gone.
	t.Run("IsIdempotent", func(t *testing.T) {
		cfg, err := NewConfigProviderFromData("")
		assert.NoError(t, err)

		loadAgentFrom(cfg)
		first := Agent.MaxRequestBodySize
		loadAgentFrom(cfg)

		assert.Equal(t, int64(32<<20), first, "default cap should be 32 MiB in bytes")
		assert.Equal(t, first, Agent.MaxRequestBodySize, "reloading the settings changed the cap")
	})

	t.Run("Configured", func(t *testing.T) {
		cfg, err := NewConfigProviderFromData(`
[agent]
MAX_REQUEST_BODY_SIZE = 4
`)
		assert.NoError(t, err)

		loadAgentFrom(cfg)
		assert.Equal(t, int64(4<<20), Agent.MaxRequestBodySize)

		loadAgentFrom(cfg)
		assert.Equal(t, int64(4<<20), Agent.MaxRequestBodySize)
	})

	t.Run("NonPositiveFallsBackToDefault", func(t *testing.T) {
		cfg, err := NewConfigProviderFromData(`
[agent]
MAX_REQUEST_BODY_SIZE = 0
`)
		assert.NoError(t, err)

		loadAgentFrom(cfg)
		assert.Equal(t, int64(32<<20), Agent.MaxRequestBodySize)

		loadAgentFrom(cfg)
		assert.Equal(t, int64(32<<20), Agent.MaxRequestBodySize)
	})
}
