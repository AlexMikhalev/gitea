// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package swagger

import api "code.gitea.io/gitea/modules/structs"

// AgentKey
// swagger:response AgentKey
type swaggerResponseAgentKey struct {
	// in:body
	Body api.AgentKey `json:"body"`
}

// AgentKeyList
// swagger:response AgentKeyList
type swaggerResponseAgentKeyList struct {
	// in:body
	Body []api.AgentKey `json:"body"`
}

// AgentAuditEventList
// swagger:response AgentAuditEventList
type swaggerResponseAgentAuditEventList struct {
	// in:body
	Body []api.AgentAuditEvent `json:"body"`
}
