// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Declarative rules: trigger → **allowlisted action**.
//!
//! The action space is a closed enum: `create_task`, `comment`, `label`, `open_pr`,
//! `unblock`. There is no shell variant, and no way to spell one — an unknown action name
//! is a named parse error, not an escape hatch. This is the one place a config file could
//! otherwise turn into arbitrary code execution, so the enum is the security boundary and
//! it is tested as such.
//!
//! This is deliberately *not* a second rules engine. Scheduling, retry and liveness stay in
//! kanban; these rules only choose which of five bridge actions fires.
//!
//! **Scaffolding, not wiring.** Nothing in this module is consulted at runtime today. The
//! only callers of [`RuleSet::load`] are `check-rules` and the config validation in
//! `main.rs`, both of which parse and discard. What actually drives the daemon is the
//! config: `poll.ready_interval_secs` sets the ready cadence — *not* a `schedule` rule's
//! `every_secs` — and the outbound mapping in [`crate::outbound`] chooses actions directly.
//! The closed action space is a real property of that mapping (`PlannedAction::action` is
//! total onto [`Action`]); it is not something this file enforces on a running daemon.
//! Anything written here is validated and then ignored, so say so wherever it is offered
//! to an operator — see [`RuleSet::VALIDATION_ONLY_NOTICE`].

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

/// Every action the bridge will ever perform, by name.
///
/// Adding to this list is a code change with a review. Nothing in a YAML file can extend
/// it.
pub const ALLOWED_ACTIONS: [&str; 5] = ["create_task", "comment", "label", "open_pr", "unblock"];

/// The closed action allowlist.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Action {
    /// Create a kanban task from a Gitea issue.
    CreateTask,
    /// Post an issue comment.
    Comment,
    /// Add labels to an issue.
    Label,
    /// Open a pull request.
    OpenPr,
    /// Return a blocked task to ready.
    Unblock,
}

impl Action {
    /// Resolves an action name against the allowlist.
    pub fn from_name(name: &str) -> Result<Self, RulesError> {
        match name {
            "create_task" => Ok(Self::CreateTask),
            "comment" => Ok(Self::Comment),
            "label" => Ok(Self::Label),
            "open_pr" => Ok(Self::OpenPr),
            "unblock" => Ok(Self::Unblock),
            other => Err(RulesError::UnknownAction {
                name: other.to_string(),
                allowed: ALLOWED_ACTIONS.join(", "),
            }),
        }
    }

    /// The canonical name of this action.
    pub fn name(self) -> &'static str {
        match self {
            Self::CreateTask => "create_task",
            Self::Comment => "comment",
            Self::Label => "label",
            Self::OpenPr => "open_pr",
            Self::Unblock => "unblock",
        }
    }
}

/// What causes a rule to fire.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Trigger {
    /// Fire every `every_secs` seconds.
    Schedule {
        /// Interval in seconds.
        every_secs: u64,
    },
    /// Fire on an inbound webhook of the named event.
    Webhook {
        /// Event name, e.g. `issues`.
        event: String,
    },
    /// Fire when a reaction alias appears on an issue.
    Reaction {
        /// Emoji **alias**, e.g. `honeybee` — never a codepoint.
        content: String,
    },
}

impl Trigger {
    /// The trigger's kind name, for logs.
    pub fn kind(&self) -> &'static str {
        match self {
            Self::Schedule { .. } => "schedule",
            Self::Webhook { .. } => "webhook",
            Self::Reaction { .. } => "reaction",
        }
    }
}

/// One rule.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Rule {
    /// Rule name, used in logs.
    pub name: String,
    /// What fires it.
    pub trigger: Trigger,
    /// What it does. Always one of [`ALLOWED_ACTIONS`].
    pub action: Action,
    /// Free-form action parameters, validated by whoever consumes them.
    pub with: std::collections::BTreeMap<String, String>,
}

/// A parsed rules file.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RuleSet {
    /// The rules, in file order.
    pub rules: Vec<Rule>,
}

impl RuleSet {
    /// What every operator-facing report of a rules file must also say.
    ///
    /// A valid rules file changes nothing about a running daemon, and `rules.example.yaml`
    /// looks enough like a live schedule (`poll-ready-issues / every_secs: 60`) that saying
    /// only "3 rule(s) OK" would read as confirmation that it is driving something.
    pub const VALIDATION_ONLY_NOTICE: &'static str = concat!(
        "note: rules are validated only. Nothing consults them at runtime; ",
        "the ready cadence is poll.ready_interval_secs, and outbound actions ",
        "come from src/outbound.rs"
    );

    /// Parses a rules document.
    pub fn parse(yaml: &str) -> Result<Self, RulesError> {
        let raw: RawRuleSet = serde_yaml::from_str(yaml)?;
        let mut rules = Vec::with_capacity(raw.rules.len());
        let mut seen = std::collections::BTreeSet::new();
        for r in raw.rules {
            if r.name.trim().is_empty() {
                return Err(RulesError::MissingName);
            }
            if !seen.insert(r.name.clone()) {
                return Err(RulesError::DuplicateName { name: r.name });
            }
            // The allowlist is consulted by *name*, so an unknown action can only ever be
            // an error — never a fallthrough to something executable.
            let action = Action::from_name(r.action.trim())?;
            let trigger = r.trigger.into_trigger(&r.name)?;
            rules.push(Rule {
                name: r.name,
                trigger,
                action,
                with: r.with,
            });
        }
        Ok(Self { rules })
    }

    /// Loads and parses a rules file.
    pub fn load(path: &Path) -> Result<Self, RulesError> {
        let raw = std::fs::read_to_string(path).map_err(|source| RulesError::Read {
            path: path.to_path_buf(),
            source,
        })?;
        Self::parse(&raw)
    }

    /// The rules that fire on the given trigger kind.
    pub fn by_trigger_kind(&self, kind: &str) -> Vec<&Rule> {
        self.rules.iter().filter(|r| r.trigger.kind() == kind).collect()
    }
}

/// Rules-file failures.
#[derive(Debug, thiserror::Error)]
pub enum RulesError {
    /// The action name is not on the allowlist. This is the error acceptance criterion 4
    /// asks for by name.
    #[error("unknown action {name:?}: rules may only use one of [{allowed}]")]
    UnknownAction {
        /// The rejected name.
        name: String,
        /// The allowlist, for the operator.
        allowed: String,
    },
    /// The trigger type is not one of `schedule | webhook | reaction`.
    #[error("rule {rule:?} has unknown trigger type {name:?}: expected one of [schedule, webhook, reaction]")]
    UnknownTrigger {
        /// Owning rule.
        rule: String,
        /// The rejected type.
        name: String,
    },
    /// A trigger is missing the field its type requires.
    #[error("rule {rule:?}: {trigger} trigger requires {field:?}")]
    MissingTriggerField {
        /// Owning rule.
        rule: String,
        /// Trigger type.
        trigger: String,
        /// Missing field.
        field: String,
    },
    /// A rule has no name.
    #[error("every rule needs a name")]
    MissingName,
    /// Two rules share a name.
    #[error("duplicate rule name {name:?}")]
    DuplicateName {
        /// The repeated name.
        name: String,
    },
    /// The document is not valid YAML, or not the expected shape.
    #[error("cannot parse rules: {0}")]
    Yaml(#[from] serde_yaml::Error),
    /// The file could not be read.
    #[error("cannot read rules {path}: {source}")]
    Read {
        /// Path.
        path: PathBuf,
        /// Underlying error.
        #[source]
        source: std::io::Error,
    },
}

/// The on-disk shape.
///
/// The action arrives as a plain `String` on purpose. Deserialising straight into [`Action`]
/// would work, but the error would be serde's — this way the allowlist itself produces the
/// message, and the allowlist is what we want operators to read.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawRuleSet {
    #[serde(default)]
    rules: Vec<RawRule>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawRule {
    name: String,
    trigger: RawTrigger,
    action: String,
    #[serde(default)]
    with: std::collections::BTreeMap<String, String>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawTrigger {
    #[serde(rename = "type")]
    kind: String,
    #[serde(default)]
    every_secs: Option<u64>,
    #[serde(default)]
    event: Option<String>,
    #[serde(default)]
    content: Option<String>,
}

impl RawTrigger {
    fn into_trigger(self, rule: &str) -> Result<Trigger, RulesError> {
        match self.kind.trim() {
            "schedule" => Ok(Trigger::Schedule {
                every_secs: self.every_secs.ok_or_else(|| RulesError::MissingTriggerField {
                    rule: rule.to_string(),
                    trigger: "schedule".into(),
                    field: "every_secs".into(),
                })?,
            }),
            "webhook" => Ok(Trigger::Webhook {
                event: self.event.ok_or_else(|| RulesError::MissingTriggerField {
                    rule: rule.to_string(),
                    trigger: "webhook".into(),
                    field: "event".into(),
                })?,
            }),
            "reaction" => Ok(Trigger::Reaction {
                content: self.content.ok_or_else(|| RulesError::MissingTriggerField {
                    rule: rule.to_string(),
                    trigger: "reaction".into(),
                    field: "content".into(),
                })?,
            }),
            other => Err(RulesError::UnknownTrigger {
                rule: rule.to_string(),
                name: other.to_string(),
            }),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_allowlisted_action_parses() {
        for name in ALLOWED_ACTIONS {
            let action = Action::from_name(name).expect("allowlisted");
            assert_eq!(action.name(), name);
        }
    }

    #[test]
    fn the_allowlist_has_no_shell_variant() {
        // Acceptance criterion 4, stated as a property: the action space is exactly these
        // five names, so no rules file can ever reach a shell.
        assert_eq!(ALLOWED_ACTIONS.len(), 5);
        for forbidden in ["shell", "exec", "run", "sh", "bash", "command", "script", "eval"] {
            let err = Action::from_name(forbidden).expect_err("must be rejected");
            let msg = err.to_string();
            assert!(
                msg.contains(forbidden),
                "error must name the rejected action: {msg}"
            );
            assert!(
                msg.contains("create_task"),
                "error must show the allowlist: {msg}"
            );
        }
    }

    #[test]
    fn a_full_ruleset_parses() {
        let yaml = r#"
rules:
  - name: poll-ready
    trigger: {type: schedule, every_secs: 60}
    action: create_task
  - name: approve
    trigger: {type: reaction, content: honeybee}
    action: unblock
  - name: on-push
    trigger: {type: webhook, event: issues}
    action: label
    with: {labels: "status/blocked"}
"#;
        let set = RuleSet::parse(yaml).expect("parses");
        assert_eq!(set.rules.len(), 3);
        assert_eq!(set.rules[0].action, Action::CreateTask);
        assert_eq!(set.rules[0].trigger, Trigger::Schedule { every_secs: 60 });
        assert_eq!(
            set.rules[1].trigger,
            Trigger::Reaction {
                content: "honeybee".into()
            }
        );
        assert_eq!(
            set.rules[2].with.get("labels").map(String::as_str),
            Some("status/blocked")
        );
        assert_eq!(set.by_trigger_kind("reaction").len(), 1);
    }

    #[test]
    fn a_shell_action_fails_to_parse_with_a_named_error() {
        let yaml = "rules:\n  - name: pwn\n    trigger: {type: schedule, every_secs: 1}\n    action: shell\n";
        let err = RuleSet::parse(yaml).expect_err("must reject");
        assert!(matches!(err, RulesError::UnknownAction { .. }), "{err:?}");
        assert!(err.to_string().contains("shell"), "{err}");
    }

    #[test]
    fn an_action_carrying_a_command_still_fails() {
        // Not a special case — `sh -c ...` is simply not on the allowlist either.
        let yaml = "rules:\n  - name: pwn\n    trigger: {type: schedule, every_secs: 1}\n    action: \"sh -c 'id'\"\n";
        let err = RuleSet::parse(yaml).expect_err("must reject");
        assert!(matches!(err, RulesError::UnknownAction { .. }), "{err:?}");
    }

    #[test]
    fn unknown_top_level_keys_are_rejected() {
        // `deny_unknown_fields` means a typo cannot silently become a no-op rule, and an
        // injected `command:` key cannot ride along unnoticed.
        let yaml = "rules:\n  - name: r\n    trigger: {type: schedule, every_secs: 1}\n    action: comment\n    command: id\n";
        let err = RuleSet::parse(yaml).expect_err("must reject");
        assert!(matches!(err, RulesError::Yaml(_)), "{err:?}");
    }

    #[test]
    fn unknown_trigger_type_is_named() {
        let yaml = "rules:\n  - name: r\n    trigger: {type: cron}\n    action: comment\n";
        let err = RuleSet::parse(yaml).expect_err("must reject");
        assert!(err.to_string().contains("cron"), "{err}");
    }

    #[test]
    fn missing_trigger_field_is_named() {
        let yaml = "rules:\n  - name: r\n    trigger: {type: schedule}\n    action: comment\n";
        let err = RuleSet::parse(yaml).expect_err("must reject");
        assert!(err.to_string().contains("every_secs"), "{err}");
    }

    #[test]
    fn duplicate_rule_names_are_rejected() {
        let yaml = "rules:\n  - name: r\n    trigger: {type: schedule, every_secs: 1}\n    action: comment\n  - name: r\n    trigger: {type: schedule, every_secs: 2}\n    action: label\n";
        assert!(matches!(
            RuleSet::parse(yaml),
            Err(RulesError::DuplicateName { .. })
        ));
    }

    #[test]
    fn an_empty_document_is_an_empty_ruleset() {
        assert_eq!(RuleSet::parse("rules: []").expect("parses").rules.len(), 0);
    }

    #[test]
    fn load_reports_the_path() {
        let err = RuleSet::load(Path::new("/nonexistent/rules.yaml")).expect_err("must fail");
        assert!(err.to_string().contains("/nonexistent/rules.yaml"), "{err}");
    }
}
