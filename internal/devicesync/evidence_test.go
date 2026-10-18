package devicesync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

const evidenceNativeSession = "01234567-89ab-4cde-8f01-234567890abc"

func nativeEvidenceSpec(e *env, name string) SourceSpec {
	sp := e.spec(name, transcript.StorageJSONLAppend)
	sp.Agent = transcript.AgentClaude
	sp.Parser = "claude@1"
	sp.SessionKey = evidenceNativeSession
	return sp
}

func TestCaptureEvidenceFindsAckedLegacyHistoryBeforeRPC(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := nativeEvidenceSpec(e, "legacy.jsonl")
	data := jsonlLines(97, 20, 100)
	appendFile(t, sp.Path, data)
	if err := e.sy.Sync(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	pending, err := e.store.PendingSpecs(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("expected acknowledged legacy generation: %+v,%v", pending, err)
	}
	// References come from actual stored generations, even after source removal.
	if err := os.Remove(sp.Path); err != nil {
		t.Fatal(err)
	}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	evidence, err := sc.CaptureEvidence(context.Background(), sp.Agent, sp.SessionKey)
	if err != nil || !evidence.Unproven || len(evidence.Sources) != 1 || len(evidence.RestrictionSources) != 0 {
		t.Fatalf("legacy evidence=%+v,%v", evidence, err)
	}
	ref := evidence.Sources[0]
	if ref.Path != sp.Path || ref.FileID == "" || ref.Generation != 0 {
		t.Fatalf("stored source reference=%+v", ref)
	}
	for _, origin := range []string{"cowork", "desktop-code"} {
		strict, err := sc.CaptureEvidenceForOrigin(context.Background(), sp.Agent, sp.SessionKey, origin)
		if err != nil || !strict.Unproven || len(strict.Sources) != 1 || strict.Sources[0] != ref {
			t.Fatalf("unqualified history origin %s: %+v,%v", origin, strict, err)
		}
	}
}

func TestCaptureEvidenceValidatesEveryGenerationProof(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := nativeEvidenceSpec(e, "controlled.jsonl")
	first := jsonlLines(98, 20, 100)
	appendFile(t, sp.Path, first)
	a := fileAuthorization(t, sp, int64(len(first)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	evidence, err := sc.CaptureEvidence(context.Background(), sp.Agent, sp.SessionKey)
	if err != nil || evidence.Unproven || len(evidence.Sources) != 1 {
		t.Fatalf("controlled evidence=%+v,%v", evidence, err)
	}
	second := jsonlLines(99, 30, 200)
	if err := os.WriteFile(sp.Path, second, 0600); err != nil {
		t.Fatal(err)
	}
	a = fileAuthorization(t, sp, int64(len(second)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	// An older acknowledged generation remains part of history; valid latest
	// capture proof must not hide its missing proof.
	if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=NULL,lost=1 WHERE generation=0`); err != nil {
		t.Fatal(err)
	}
	evidence, err = sc.CaptureEvidence(context.Background(), sp.Agent, sp.SessionKey)
	if err != nil || !evidence.Unproven || len(evidence.Sources) != 2 {
		t.Fatalf("historical proof union=%+v,%v", evidence, err)
	}
	if evidence.Sources[0].Generation != 0 || evidence.Sources[1].Generation != 1 {
		t.Fatalf("generation references=%+v", evidence.Sources)
	}
}

func TestCaptureEvidenceEmptyKeyOnlyRestrictsVerifiedCanonicalPath(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := nativeEvidenceSpec(e, evidenceNativeSession+".jsonl")
	sp.SessionKey = ""
	appendFile(t, sp.Path, jsonlLines(100, 10, 100))
	if err := e.sy.Sync(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	evidence, err := sc.CaptureEvidence(context.Background(), sp.Agent, evidenceNativeSession)
	if err != nil || evidence.Unproven || len(evidence.Sources)+len(evidence.RestrictionSources) != 0 {
		t.Fatalf("unverified path inferred association: %+v,%v", evidence, err)
	}
	evidence, err = sc.CaptureEvidence(context.Background(), sp.Agent, evidenceNativeSession, sp.Path)
	if err != nil || !evidence.Unproven || len(evidence.Sources) != 0 || len(evidence.RestrictionSources) != 1 {
		t.Fatalf("canonical restriction association=%+v,%v", evidence, err)
	}
	// Explicit paths still cannot associate a different session identity.
	if out, err := sc.CaptureEvidence(context.Background(), sp.Agent, "different-session", sp.Path); err == nil {
		t.Fatalf("mismatched canonical path accepted: %+v", out)
	}
}

func TestCaptureEvidenceExcludesRecoveryExports(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := nativeEvidenceSpec(e, "cass-export.jsonl")
	sp.StorageKind = "cass_export"
	sp.Parser = "cass@1"
	appendFile(t, sp.Path, jsonlLines(101, 10, 100))
	if err := e.sy.Sync(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	out, err := sc.CaptureEvidence(context.Background(), sp.Agent, sp.SessionKey, sp.Path)
	if err != nil || out.Unproven || len(out.Sources)+len(out.RestrictionSources) != 0 {
		t.Fatalf("CASS export became native association: %+v,%v", out, err)
	}
}

func TestCaptureEvidenceFailsClosedOnMalformedProofOrIdentity(t *testing.T) {
	for _, kind := range []string{"malformed proof", "mismatched spec path", "mismatched session", "mismatched agent", "invalid bound"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := nativeEvidenceSpec(e, "corrupt.jsonl")
			data := jsonlLines(102, 10, 100)
			appendFile(t, sp.Path, data)
			a := fileAuthorization(t, sp, int64(len(data)))
			if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
				t.Fatal(err)
			}
			var query string
			switch kind {
			case "malformed proof":
				query = `UPDATE devsync_gens SET capture_proof='{'`
			case "mismatched spec path":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.Path','/wrong/path')`
			case "mismatched session":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.SessionKey','different-session')`
			case "mismatched agent":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.Agent','codex')`
			case "invalid bound":
				query = `UPDATE devsync_gens SET capture_proof=json_set(capture_proof,'$.Offset',0)`
			}
			if _, err := e.store.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			sc := NewScheduler(e.sy, SchedulerConfig{})
			out, err := sc.CaptureEvidence(context.Background(), sp.Agent, sp.SessionKey, sp.Path)
			if kind == "invalid bound" {
				if err != nil || !out.Unproven {
					t.Fatalf("invalid bound not held: %+v,%v", out, err)
				}
			} else if err == nil {
				t.Fatalf("malformed association accepted: %+v", out)
			}
		})
	}
}

func TestCaptureEvidenceRejectsNonNativeParserStorageAndWeakCompanionAncestor(t *testing.T) {
	for _, kind := range []string{"empty parser", "wrong parser", "empty version", "wrong storage", "weak companion ancestor"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := nativeEvidenceSpec(e, "native.jsonl")
			if kind == "weak companion ancestor" {
				sp.Path = e.path(evidenceNativeSession + "/arbitrary/folder/result.txt")
				if err := os.MkdirAll(filepath.Dir(sp.Path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			appendFile(t, sp.Path, jsonlLines(103, 10, 100))
			if err := e.sy.Sync(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
			var query string
			switch kind {
			case "empty parser":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.Parser','')`
			case "wrong parser":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.Parser','codex@1')`
			case "empty version":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.Parser','claude@')`
			case "wrong storage":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.StorageKind','json_doc')`
			case "weak companion ancestor":
				query = `UPDATE devsync_sources SET spec=json_set(spec,'$.StorageKind','companion','$.SessionKey','','$.Parent','/different/session.jsonl')`
			}
			if _, err := e.store.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			sc := NewScheduler(e.sy, SchedulerConfig{})
			if out, err := sc.CaptureEvidence(context.Background(), transcript.AgentClaude, evidenceNativeSession, sp.Path); err == nil {
				t.Fatalf("nonnative source association accepted: %+v", out)
			}
		})
	}
}

func TestCaptureEvidenceCanonicalCompanionRestriction(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		t.Run(fmt.Sprint("sidecar=", sidecar), func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			parent := e.path(evidenceNativeSession + ".jsonl")
			path := e.path(evidenceNativeSession + "/tool-results/result.txt")
			if sidecar {
				path = e.path(evidenceNativeSession + ".meta.json")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			sp := nativeEvidenceSpec(e, "unused")
			sp.Path = path
			sp.Parent = parent
			sp.StorageKind = transcript.StorageCompanion
			sp.SessionKey = ""
			appendFile(t, path, []byte("synthetic tool result"))
			if err := e.sy.Sync(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
			sc := NewScheduler(e.sy, SchedulerConfig{})
			out, err := sc.CaptureEvidence(context.Background(), transcript.AgentClaude, evidenceNativeSession, path)
			if err != nil || !out.Unproven || len(out.RestrictionSources) != 1 || len(out.Sources) != 0 {
				t.Fatalf("canonical companion restriction=%+v,%v", out, err)
			}
		})
	}
}

func TestCaptureEvidenceForOriginDoesNotRecertifyOtherOrigin(t *testing.T) {
	for _, capturedOrigin := range []string{"desktop-code", "cowork"} {
		t.Run(capturedOrigin, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := nativeEvidenceSpec(e, "origin.jsonl")
			data := jsonlLines(116, 20, 100)
			appendFile(t, sp.Path, data)
			a := fileAuthorization(t, sp, int64(len(data)))
			a.Origin = capturedOrigin
			if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
				t.Fatal(err)
			}
			sc := NewScheduler(e.sy, SchedulerConfig{})
			generic, err := sc.CaptureEvidence(context.Background(), sp.Agent, sp.SessionKey)
			if err != nil || generic.Unproven || len(generic.Sources) != 1 {
				t.Fatalf("generic capture fact: %+v,%v", generic, err)
			}
			for _, expectedOrigin := range []string{"desktop-code", "cowork"} {
				evidence, err := sc.CaptureEvidenceForOrigin(context.Background(), sp.Agent, sp.SessionKey, expectedOrigin)
				if err != nil || evidence.Unproven != (expectedOrigin != capturedOrigin) || len(evidence.Sources) != 1 || evidence.Sources[0] != generic.Sources[0] {
					t.Fatalf("origin %s evidence: %+v,%v", expectedOrigin, evidence, err)
				}
			}
			// Qualification for either origin must still hold when an old nonempty
			// capture lacks proof, even if the source's durable marker is qualified.
			if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=NULL`); err != nil {
				t.Fatal(err)
			}
			for _, expectedOrigin := range []string{"desktop-code", "cowork"} {
				evidence, err := sc.CaptureEvidenceForOrigin(context.Background(), sp.Agent, sp.SessionKey, expectedOrigin)
				if err != nil || !evidence.Unproven || len(evidence.Sources) != 1 {
					t.Fatalf("legacy origin %s evidence: %+v,%v", expectedOrigin, evidence, err)
				}
			}
		})
	}
}

func TestCaptureEvidenceForOriginRejectsUnknownOrigin(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	for _, origin := range []string{"", "cass", "unknown"} {
		if out, err := sc.CaptureEvidenceForOrigin(context.Background(), transcript.AgentClaude, evidenceNativeSession, origin); err == nil {
			t.Fatalf("invalid origin accepted: %+v", out)
		}
	}
}
