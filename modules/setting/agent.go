// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import "code.gitea.io/gitea/modules/log"

// Agent settings govern NIP-98 signed agent requests (see services/agentauth).
var Agent = struct {
	MaxRequestBodySize int64
}{
	MaxRequestBodySize: 32,
}

func loadAgentFrom(rootCfg ConfigProvider) {
	mustMapSetting(rootCfg, "agent", &Agent)

	// A NIP-98 signature commits to a hash of the whole body, so the body has to be buffered
	// before the handler runs - it cannot be streamed and checked incrementally. That buffer is
	// the one cost of a signed request an operator cannot otherwise bound, and N concurrent
	// requests from one registered key hold N of them resident. Making the ceiling a setting is
	// what turns "32 MiB times however many requests arrive" into a number the operator picked;
	// a deployment whose agents only ever post JSON can drop it to single-digit MiB and cap the
	// exposure, and one that needs larger uploads can raise it knowingly.
	if Agent.MaxRequestBodySize <= 0 {
		log.Warn("[agent].MAX_REQUEST_BODY_SIZE must be positive, falling back to 32 MiB")
		Agent.MaxRequestBodySize = 32
	}

	// Configured in MiB, used in bytes.
	Agent.MaxRequestBodySize = 1 << 20 * Agent.MaxRequestBodySize
}
