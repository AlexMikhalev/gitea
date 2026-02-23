// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"fmt"
	"time"

	"code.gitea.io/gitea/modules/log"
)

// AuditEvent represents a single robot API access event
type AuditEvent struct {
	UserID    int64     // User ID (0 for anonymous)
	Username  string    // Username ("anonymous" for unauthenticated)
	Owner     string    // Repository owner
	Repo      string    // Repository name
	Endpoint  string    // API endpoint accessed
	RemoteIP  string    // Client IP address
	Timestamp time.Time // Event timestamp
	Success   bool      // Whether access was granted
	Reason    string    // Reason for denial (if Success=false)
}

// LogRobotAccess logs a robot API access event to the audit log
// This function should be called after all security checks have been performed
func LogRobotAccess(event *AuditEvent) {
	if event == nil {
		log.Warn("LogRobotAccess called with nil event")
		return
	}

	// Ensure timestamp is set
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	// Format the log entry
	// Format: [ROBOT_AUDIT] user=username(uid) repo=owner/repo endpoint=path ip=remote_ip success=true/false reason=optional
	status := "SUCCESS"
	if !event.Success {
		status = "DENIED"
	}

	userID := event.UserID
	if userID == 0 {
		userID = 0
	}

	username := event.Username
	if username == "" {
		if event.UserID == 0 {
			username = "anonymous"
		} else {
			username = fmt.Sprintf("uid:%d", event.UserID)
		}
	}

	// Build log message
	logMsg := fmt.Sprintf(
		"[ROBOT_AUDIT] status=%s user=%s(uid=%d) repo=%s/%s endpoint=%s ip=%s timestamp=%s",
		status,
		username,
		userID,
		event.Owner,
		event.Repo,
		event.Endpoint,
		event.RemoteIP,
		event.Timestamp.Format(time.RFC3339),
	)

	if !event.Success && event.Reason != "" {
		logMsg = fmt.Sprintf("%s reason=%s", logMsg, event.Reason)
	}

	// Log at INFO level for visibility
	// In production, this can be redirected to a separate audit log file
	log.Info(logMsg)
}

// LogRobotAccessQuick is a convenience function for common audit logging scenarios
// It creates and logs an AuditEvent with the provided parameters
func LogRobotAccessQuick(
	userID int64,
	username string,
	owner string,
	repo string,
	endpoint string,
	remoteIP string,
	success bool,
	reason string,
) {
	event := &AuditEvent{
		UserID:    userID,
		Username:  username,
		Owner:     owner,
		Repo:      repo,
		Endpoint:  endpoint,
		RemoteIP:  remoteIP,
		Timestamp: time.Now(),
		Success:   success,
		Reason:    reason,
	}
	LogRobotAccess(event)
}

// ContextInterface defines the interface needed from Gitea's context
// This is typically satisfied by *context.Context or *APIContext
// Note: In real implementation, replace with actual Gitea context type
type ContextInterface interface {
	IsSigned() bool
	GetDoer() UserInterface
	GetRemoteAddr() string
}

// UserInterface defines the interface needed from Gitea's user model
type UserInterface interface {
	GetID() int64
	GetName() string
}

// LogRobotAccessFromContext logs robot access using context-derived information
// This is the preferred method when using Gitea's context
func LogRobotAccessFromContext(
	ctx ContextInterface,
	owner string,
	repo string,
	endpoint string,
	success bool,
	reason string,
) {
	var userID int64
	var username string

	if ctx != nil && ctx.IsSigned() && ctx.GetDoer() != nil {
		userID = ctx.GetDoer().GetID()
		username = ctx.GetDoer().GetName()
	} else {
		userID = 0
		username = "anonymous"
	}

	remoteIP := ""
	if ctx != nil {
		remoteIP = ctx.GetRemoteAddr()
	}

	LogRobotAccessQuick(userID, username, owner, repo, endpoint, remoteIP, success, reason)
}
