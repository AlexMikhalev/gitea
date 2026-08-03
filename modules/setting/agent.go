// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import "code.gitea.io/gitea/modules/log"

const defaultAgentMaxRequestBodySizeMiB = 32

// agentConfig holds the [agent] section exactly as the operator wrote it - MAX_REQUEST_BODY_SIZE
// in MiB. It is separate from Agent (which carries the derived byte value) because mustMapSetting
// leaves a field untouched when the section is absent, which is the shipping default: mapping and
// converting in place would multiply the already-converted value by 1 MiB again on every reload,
// and LoadCommonSettings does run twice in one process (the installer path). Keeping the source
// value here makes loadAgentFrom idempotent - see TestLoadAgentFromIsIdempotent.
var agentConfig = struct {
	MaxRequestBodySize int64
}{
	MaxRequestBodySize: defaultAgentMaxRequestBodySizeMiB,
}

// Agent settings govern NIP-98 signed agent requests (see services/agentauth).
// MaxRequestBodySize is in bytes, derived from the MiB value the operator configured.
var Agent = struct {
	MaxRequestBodySize int64
}{
	MaxRequestBodySize: defaultAgentMaxRequestBodySizeMiB << 20,
}

func loadAgentFrom(rootCfg ConfigProvider) {
	mustMapSetting(rootCfg, "agent", &agentConfig)

	// A NIP-98 signature commits to a hash of the whole body, so the body has to be buffered
	// before the handler runs - it cannot be streamed and checked incrementally. That buffer is
	// the one cost of a signed request an operator cannot otherwise bound, and N concurrent
	// requests from one registered key hold N of them resident. Making the ceiling a setting is
	// what turns "32 MiB times however many requests arrive" into a number the operator picked;
	// a deployment whose agents only ever post JSON can drop it to single-digit MiB and cap the
	// exposure, and one that needs larger uploads can raise it knowingly.
	if agentConfig.MaxRequestBodySize <= 0 {
		log.Warn("[agent].MAX_REQUEST_BODY_SIZE must be positive, falling back to 32 MiB")
		agentConfig.MaxRequestBodySize = defaultAgentMaxRequestBodySizeMiB
	}

	// Configured in MiB, used in bytes.
	Agent.MaxRequestBodySize = 1 << 20 * agentConfig.MaxRequestBodySize
}
