// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! gitea-automations bridge daemon (issue #57, epic #53, phase F4).
//!
//! Gitea holds the shared board (PageRank triage, dependency graph, human review);
//! Hermes kanban holds the execution fabric (atomic claims, heartbeats, crash reclaim,
//! circuit breaker, isolated workspaces, `task_events` audit). This crate is the bridge
//! between them — not a second rules engine. The fabric is inherited, not rebuilt:
//!
//! * [`inbound`] polls `GET /api/v1/robot/ready` and turns each ready issue into exactly
//!   one kanban task, deduplicated by `--idempotency-key gitea:<owner>/<repo>#<index>`.
//! * [`outbound`] watches the kanban event stream for the five terminal kinds and maps
//!   them back onto Gitea: `completed` opens a PR, the other four apply `status/blocked`.
//! * [`rules`] parses declarative YAML rules whose action space is a closed allowlist —
//!   there is no shell action, and no way to spell one.
//! * [`approval`] polls 🐝 (`honeybee`) reactions as the human approval signal, because
//!   reactions are not observable through the F2 event stream.

pub mod approval;
pub mod config;
pub mod gitea;
pub mod hermes;
pub mod inbound;
pub mod outbound;
pub mod robot;
pub mod rules;
