// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! The bridge's write leg, checked against a real `gitea-robot` binary.
//!
//! The argv tests in `src/robot.rs` assert only that the bridge agrees with *itself* about
//! the commands it builds. That is what let the whole outbound leg call subcommands the CLI
//! did not have. Two checks close that gap, and neither replaces the other:
//!
//! * `TestBridgeWriteVerbsExist` (`cmd/gitea-robot/write_test.go`) — always runs, in Go CI,
//!   and asserts the verbs and flags exist in the dispatch table.
//! * this file — runs a real binary, so it also covers the case where the CLI builds but
//!   does not accept the argv the bridge hands it.
//!
//! It needs a built binary and therefore opts in:
//!
//! ```text
//! go build -o /tmp/gitea-robot ./cmd/gitea-robot
//! GITEA_ROBOT_BIN=/tmp/gitea-robot cargo test --manifest-path crates/Cargo.toml \
//!     --test robot_cli_contract -- --ignored
//! ```
//!
//! Full end-to-end dogfooding — a 🐝 on a real issue driving a real pull request — is still
//! unimplemented; it needs a reachable instance and a NIP-98 agent key.

use std::process::Command;

use bridge::config::RobotConfig;
use bridge::robot::{PullRequest, Robot};

/// The binary under test, or `None` when the opt-in variable is unset.
///
/// `GITEA_ROBOT_BIN` is required rather than falling back to `PATH`: a `gitea-robot` that
/// happens to be installed may be an older build than the tree, and a stale pass here is
/// worse than no test.
fn robot_binary() -> Option<String> {
    match std::env::var("GITEA_ROBOT_BIN") {
        Ok(path) if !path.trim().is_empty() => Some(path),
        _ => {
            eprintln!("skipping: set GITEA_ROBOT_BIN to a built cmd/gitea-robot binary");
            None
        }
    }
}

/// Runs `gitea-robot <verb> --help`, which parses flags and exits without making a request.
fn help_exit_code(binary: &str, verb: &str) -> i32 {
    let out = Command::new(binary)
        .args([verb, "--help"])
        // main() requires a credential before it dispatches at all. Any value will do:
        // --help never reaches a request.
        .env("GITEA_TOKEN", "contract-test")
        .output()
        .unwrap_or_else(|err| panic!("cannot run {binary} {verb} --help: {err}"));
    out.status.code().unwrap_or(-1)
}

/// Every verb the bridge emits must be a verb the CLI knows. An unknown command exits 1
/// with `Unknown command:` (`cmd/gitea-robot/main.go`), which is exactly the failure the
/// outbound leg hit on every terminal event.
#[test]
#[ignore = "needs a built gitea-robot; set GITEA_ROBOT_BIN"]
fn every_verb_the_bridge_emits_is_accepted() {
    let Some(binary) = robot_binary() else { return };
    let robot = Robot::new(&RobotConfig::default());

    let pr = PullRequest {
        title: "issue #57: automations daemon".into(),
        head: "task/57-automations-daemon".into(),
        body: "Refs #57".into(),
    };
    let plans = [
        robot.comment_args("terraphim", "gitea", 57, "audit note"),
        robot.add_labels_args("terraphim", "gitea", 57, &["status/blocked".to_string()]),
        robot.create_pull_args("terraphim", "gitea", &pr),
    ];

    for argv in &plans {
        let verb = &argv[0];
        assert_eq!(
            help_exit_code(&binary, verb),
            0,
            "gitea-robot {verb} --help failed; the bridge emits {verb} on every terminal event"
        );
    }
}

/// The flags, not just the verbs: a verb that exists but rejects `--add-labels` fails the
/// same way, one `KanbanError::Exit` at a time.
#[test]
#[ignore = "needs a built gitea-robot; set GITEA_ROBOT_BIN"]
fn every_flag_the_bridge_emits_is_declared() {
    let Some(binary) = robot_binary() else { return };
    let robot = Robot::new(&RobotConfig::default());

    let pr = PullRequest {
        title: "t".into(),
        head: "h".into(),
        body: "Refs #1".into(),
    };
    for argv in [
        robot.comment_args("o", "r", 1, "b"),
        robot.add_labels_args("o", "r", 1, &["status/blocked".to_string()]),
        robot.create_pull_args("o", "r", &pr),
    ] {
        let verb = argv[0].clone();
        // `--help` prints the flag set the verb declares; every flag the bridge passes has
        // to appear in it.
        let out = Command::new(&binary)
            .args([&verb, "--help"])
            .env("GITEA_TOKEN", "contract-test")
            .output()
            .expect("runs");
        let usage = format!(
            "{}{}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        );
        for flag in argv.iter().filter(|a| a.starts_with("--")) {
            assert!(
                usage.contains(flag.trim_start_matches('-')),
                "gitea-robot {verb} does not declare {flag}\n{usage}"
            );
        }
    }
}
