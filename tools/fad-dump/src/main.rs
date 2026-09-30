//! fad-dump: test-only oracle for Flopwire's transcript parsers.
//!
//! Adapted from gbasin/agentboard tools/fad-dump/src/main.rs
//! (MIT, Copyright (c) 2025 Gary Basin) at commit
//! 4fb640dd9b539413d2ba2fdcf5385a7b51478797. Changes from upstream:
//! pinned to franken-agent-detection =0.3.1 (git c06d1cb), emits the full
//! NormalizedConversation / NormalizedMessage (including `extra`, `snippets`
//! and `invocations`), filters by connector and source path, and writes one
//! expected file per (connector, source) holding every conversation from that
//! source. Upstream's regen script named files by source path only, so two
//! conversations from one source overwrote each other.
//!
//! Wraps franken_agent_detection (FAD), MIT, Copyright (c) 2026 Jeff Emanuel:
//! https://github.com/Dicklesworthstone/franken_agent_detection
//!
//! Connectors resolve their roots from the ambient environment, so run it as
//! `env -i HOME=<fixture home> PATH=$PATH fad-dump ...` for a clean scan.
//! Never shipped or invoked at runtime.
//!
//! Usage:
//!   fad-dump [--agent SLUG]... [--source PATH]... [--data-dir DIR]
//!            [--home DIR --out-dir DIR]
//!
//! Without --out-dir, prints one JSON object per conversation on stdout:
//!   {"connector": SLUG, "conversation": <NormalizedConversation>}
//! With --out-dir, writes <out-dir>/<connector>__<source path relative to
//! --home, '/' -> '__'>.json and removes stale *.json files there.

use franken_agent_detection::connectors::get_connector_factories;
use franken_agent_detection::{NormalizedConversation, ScanContext};
use serde_json::{json, Value};
use std::collections::BTreeMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

#[derive(Default)]
struct Args {
    agents: Vec<String>,
    sources: Vec<PathBuf>,
    data_dir: Option<PathBuf>,
    home: Option<PathBuf>,
    out_dir: Option<PathBuf>,
}

fn parse_args() -> Result<Args, String> {
    let mut args = Args::default();
    let mut it = std::env::args().skip(1);
    while let Some(flag) = it.next() {
        let mut value = || it.next().ok_or_else(|| format!("{flag} needs a value"));
        match flag.as_str() {
            "--agent" => args.agents.push(value()?),
            "--source" => args.sources.push(value()?.into()),
            "--data-dir" => args.data_dir = Some(value()?.into()),
            "--home" => args.home = Some(value()?.into()),
            "--out-dir" => args.out_dir = Some(value()?.into()),
            other => return Err(format!("unknown argument {other}")),
        }
    }
    if args.out_dir.is_some() && args.home.is_none() {
        return Err("--out-dir requires --home".into());
    }
    Ok(args)
}

fn main() -> ExitCode {
    let args = match parse_args() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("fad-dump: {e}");
            return ExitCode::from(2);
        }
    };
    let data_dir = args
        .data_dir
        .clone()
        .unwrap_or_else(|| std::env::temp_dir().join("fad-dump"));
    let ctx = ScanContext::local_default(data_dir, None);

    let mut found: Vec<(String, NormalizedConversation)> = Vec::new();
    let mut failures = 0u32;
    for (slug, make) in get_connector_factories() {
        if !args.agents.is_empty() && !args.agents.iter().any(|a| a == slug) {
            continue;
        }
        let result = make().scan_with_callback(&ctx, &mut |conv| {
            if source_selected(&args, &conv.source_path) {
                found.push((slug.to_string(), conv));
            }
            Ok(())
        });
        if let Err(err) = result {
            failures += 1;
            eprintln!("fad-dump: connector {slug} failed: {err:#}");
        }
    }
    found.sort_by(|a, b| sort_key(a).cmp(&sort_key(b)));

    let written = match (&args.out_dir, &args.home) {
        (Some(out), Some(home)) => write_expected(out, home, &found, &args.agents),
        _ => print_jsonl(&found),
    };
    if let Err(e) = written {
        eprintln!("fad-dump: {e}");
        return ExitCode::FAILURE;
    }
    if failures > 0 {
        eprintln!("fad-dump: {failures} connector(s) failed");
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}

fn source_selected(args: &Args, source: &Path) -> bool {
    if args.sources.is_empty() {
        return true;
    }
    args.sources.iter().any(|want| {
        let want = match (&args.home, want.is_relative()) {
            (Some(home), true) => home.join(want),
            _ => want.clone(),
        };
        source.starts_with(&want)
    })
}

fn sort_key(entry: &(String, NormalizedConversation)) -> (String, PathBuf, String, i64) {
    let (slug, conv) = entry;
    (
        slug.clone(),
        conv.source_path.clone(),
        conv.external_id.clone().unwrap_or_default(),
        conv.started_at.unwrap_or_default(),
    )
}

fn print_jsonl(found: &[(String, NormalizedConversation)]) -> Result<(), String> {
    let stdout = std::io::stdout();
    let mut out = stdout.lock();
    for (slug, conv) in found {
        let line = json!({"connector": slug, "conversation": conv});
        writeln!(out, "{line}").map_err(|e| e.to_string())?;
    }
    Ok(())
}

/// Name of the expected file for one (connector, relative source path).
fn expected_name(slug: &str, rel_source: &str) -> String {
    format!("{slug}__{}.json", rel_source.replace('/', "__"))
}

fn write_expected(
    out_dir: &Path,
    home: &Path,
    found: &[(String, NormalizedConversation)],
    agents: &[String],
) -> Result<(), String> {
    let home_str = home.to_string_lossy().trim_end_matches('/').to_string();
    // Group every conversation by (connector, source) so a source that yields
    // several conversations keeps all of them in one file.
    let mut groups: BTreeMap<(String, String), Vec<Value>> = BTreeMap::new();
    for (slug, conv) in found {
        let rel = match conv.source_path.strip_prefix(home) {
            Ok(r) => r.to_string_lossy().replace('\\', "/"),
            Err(_) => {
                eprintln!("fad-dump: skip source outside home: {}", conv.source_path.display());
                continue;
            }
        };
        let mut value = serde_json::to_value(conv).map_err(|e| e.to_string())?;
        value["source_path"] = Value::String(rel.clone());
        portable(&mut value, &home_str);
        groups.entry((slug.clone(), rel)).or_default().push(value);
    }

    std::fs::create_dir_all(out_dir).map_err(|e| e.to_string())?;
    let mut names: BTreeMap<String, (String, String)> = BTreeMap::new();
    for ((slug, rel), convs) in &groups {
        let name = expected_name(slug, rel);
        if let Some(prev) = names.insert(name.clone(), (slug.clone(), rel.clone())) {
            return Err(format!("expected-file name collision: {name} for {prev:?} and {rel}"));
        }
        let doc = json!({"connector": slug, "source_path": rel, "conversations": convs});
        let text = serde_json::to_string_pretty(&doc).map_err(|e| e.to_string())? + "\n";
        std::fs::write(out_dir.join(&name), text).map_err(|e| e.to_string())?;
    }
    for entry in std::fs::read_dir(out_dir).map_err(|e| e.to_string())? {
        let name = entry.map_err(|e| e.to_string())?.file_name().to_string_lossy().to_string();
        // Only prune files owned by the connectors this run regenerated, so
        // `regen-oracle.sh codex` leaves claude__*/devin__* files alone.
        let owned = agents.is_empty() || agents.iter().any(|a| name.starts_with(&format!("{a}__")));
        if owned && name.ends_with(".json") && !names.contains_key(&name) {
            std::fs::remove_file(out_dir.join(&name)).map_err(|e| e.to_string())?;
            eprintln!("fad-dump: removed stale {name}");
        }
    }
    eprintln!("fad-dump: wrote {} expected file(s)", names.len());
    Ok(())
}

/// Replaces the fixture home prefix in every string with "$HOME" so expected
/// files do not pin the machine that generated them.
fn portable(v: &mut Value, home: &str) {
    match v {
        Value::String(s) if s.contains(home) => *s = s.replace(home, "$HOME"),
        Value::Array(items) => items.iter_mut().for_each(|i| portable(i, home)),
        Value::Object(map) => map.values_mut().for_each(|i| portable(i, home)),
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expected_name_flattens_path() {
        assert_eq!(
            expected_name("devin", ".local/share/devin/cli/sessions.db/s1"),
            "devin__.local__share__devin__cli__sessions.db__s1.json"
        );
    }

    #[test]
    fn portable_rewrites_nested_strings() {
        let mut v = json!({"a": ["/h/x", {"b": "/h"}], "c": 1});
        portable(&mut v, "/h");
        assert_eq!(v, json!({"a": ["$HOME/x", {"b": "$HOME"}], "c": 1}));
    }
}
