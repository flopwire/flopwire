package ingest

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/jackc/pgx/v5"
)

// Owner metadata and observed capture are separate facts. A nil capture means
// only ownership is known. Empty FileID is an exact valid key for exports.
type policySourceIdentity struct {
	Agent, Owner string
	Capture      *policyCaptureIdentity
}

type policyCaptureIdentity struct {
	Session, Storage, ParentPath, ParentFileID, ParserFamily string
}

func loadPolicyIdentity(ctx context.Context, q policyQuerier, device string, ref syncproto.SourceRef) (policySourceIdentity, bool, error) {
	var p policySourceIdentity
	var capture policyCaptureIdentity
	var bound bool
	err := q.QueryRow(ctx, `SELECT p.owner_agent,p.owner_session_id,c.device_id IS NOT NULL,
 COALESCE(c.capture_session_key,''),COALESCE(c.storage_kind,''),COALESCE(c.parent_path,''),COALESCE(c.parent_file_id,''),COALESCE(c.parser_family,'')
 FROM source_policy_identity p LEFT JOIN source_policy_capture_identity c USING(device_id,path,file_id)
 WHERE p.device_id=$1 AND p.path=$2 AND p.file_id=$3`, device, ref.Path, ref.FileID).Scan(&p.Agent, &p.Owner, &bound, &capture.Session, &capture.Storage, &capture.ParentPath, &capture.ParentFileID, &capture.ParserFamily)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if bound {
		p.Capture = &capture
	}
	return p, err == nil, err
}

func policyIdentityConflict() error {
	return &Error{http.StatusConflict, "policy_source_identity_conflict", "protected source owner or capture identity changed; uploads remain held"}
}
func policyParserFamily(parser string) string {
	family, _, _ := strings.Cut(parser, "@")
	return family
}

// A component can contain several native owners. Exact physical parent evidence
// determines ownership; component reachability never permits reassignment.
func policyParentOwner(ctx context.Context, q policyQuerier, device string, parent syncproto.SourceRef) (agent, owner string, err error) {
	var physicalPath string
	err = q.QueryRow(ctx, `WITH RECURSIVE ancestry AS (
 SELECT id,path,file_id,agent,session_key,parent_source_id,0 AS depth FROM sources WHERE device_id=$1 AND path=$2 AND file_id=$3
 UNION SELECT s.id,s.path,s.file_id,s.agent,s.session_key,s.parent_source_id,a.depth+1 FROM sources s JOIN ancestry a ON s.id=a.parent_source_id WHERE s.device_id=$1 AND a.depth<64)
 SELECT COALESCE(p.owner_agent,a.agent),COALESCE(p.owner_session_id,a.session_key,''),a.path FROM ancestry a
 LEFT JOIN source_policy_identity p ON p.device_id=$1 AND p.path=a.path AND p.file_id=a.file_id
 WHERE p.owner_session_id IS NOT NULL OR a.parent_source_id IS NULL
 ORDER BY (p.owner_session_id IS NOT NULL) DESC,a.depth LIMIT 1`, device, parent.Path, parent.FileID).Scan(&agent, &owner, &physicalPath)
	if err == nil && owner == "" {
		owner, err = policySourceSession(ctx, q, device, syncproto.Source{Path: physicalPath, Agent: agent})
		if err == nil && owner == "" {
			err = policyIdentityConflict()
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if p, exists, e := loadPolicyIdentity(ctx, q, device, parent); e != nil {
			return "", "", e
		} else if exists {
			return p.Agent, p.Owner, nil
		}
		return "", "", policyIdentityConflict()
	}
	return agent, owner, err
}

// A protected companion can be the first physical upload. Its explicit
// canonical parent and session prove ownership only, not capture or placement.
func policyCaptureParent(ctx context.Context, q policyQuerier, device string, p policySourceIdentity, src syncproto.Source) (string, string, error) {
	agent, owner, err := policyParentOwner(ctx, q, device, *src.Parent)
	if err == nil {
		return agent, owner, nil
	}
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "policy_source_identity_conflict" {
		return "", "", err
	}
	if p.Agent != "claude" || src.Agent != p.Agent || src.SessionKey == "" || src.SessionKey != p.Owner || !validPolicySession(p.Owner) || src.Parent.FileID == "" || filepath.Base(src.Parent.Path) != p.Owner+".jsonl" {
		return "", "", policyIdentityConflict()
	}
	var existing bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sources WHERE device_id=$1 AND path=$2) OR EXISTS(SELECT 1 FROM source_policy_identity WHERE device_id=$1 AND path=$2)`, device, src.Parent.Path).Scan(&existing); err != nil {
		return "", "", err
	}
	if existing {
		return "", "", policyIdentityConflict()
	}
	return p.Agent, p.Owner, nil
}

func policyCaptureAttributes(src syncproto.Source) policyCaptureIdentity {
	c := policyCaptureIdentity{Session: src.SessionKey, Storage: src.StorageKind, ParserFamily: policyParserFamily(src.Parser)}
	if src.Parent != nil {
		c.Storage = "companion"
		c.ParentPath = src.Parent.Path
		c.ParentFileID = src.Parent.FileID
	}
	return c
}

func policyCanonicalCapture(ctx context.Context, q policyQuerier, device string, p policySourceIdentity, src syncproto.Source) (policyCaptureIdentity, error) {
	c := policyCaptureAttributes(src)
	if src.Parent != nil {
		agent, owner, err := policyCaptureParent(ctx, q, device, p, src)
		if err != nil {
			return c, err
		}
		if agent != src.Agent || (src.SessionKey != "" && src.SessionKey != owner) {
			return c, policyIdentityConflict()
		}
		c.Session = owner
	} else if src.StorageKind != "cass_export" {
		var err error
		c.Session, err = policySourceSession(ctx, q, device, src)
		if err != nil {
			return c, err
		}
	}
	return c, nil
}

func checkPolicyCapture(ctx context.Context, q policyQuerier, device string, p policySourceIdentity, src syncproto.Source) error {
	c, err := policyCanonicalCapture(ctx, q, device, p, src)
	if err != nil {
		return err
	}
	if src.Agent != p.Agent || c.Storage == "" || (c.Storage == "companion" && c.ParentPath == "") {
		return policyIdentityConflict()
	}
	recoveredAlias := p.Capture != nil && p.Capture.Storage == "cass_export" && c.Session == p.Capture.Session
	if src.Parent != nil {
		agent, owner, err := policyCaptureParent(ctx, q, device, p, src)
		if err != nil {
			return err
		}
		if agent != p.Agent || owner != p.Owner || (c.Session != "" && c.Session != p.Owner) {
			return policyIdentityConflict()
		}
	} else if c.Session != p.Owner && !recoveredAlias {
		return policyIdentityConflict()
	}
	if old := p.Capture; old != nil && (old.Storage != c.Storage || old.ParentPath != c.ParentPath || old.ParentFileID != c.ParentFileID || old.Session != c.Session || (old.ParserFamily != "" && old.ParserFamily != c.ParserFamily)) {
		return policyIdentityConflict()
	}
	return nil
}

func insertPolicyCapture(ctx context.Context, tx pgx.Tx, device string, ref syncproto.SourceRef, c policyCaptureIdentity) error {
	tag, err := tx.Exec(ctx, `INSERT INTO source_policy_capture_identity(device_id,path,file_id,capture_session_key,storage_kind,parent_path,parent_file_id,parser_family)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(device_id,path,file_id) DO UPDATE SET parser_family=CASE WHEN source_policy_capture_identity.parser_family='' THEN EXCLUDED.parser_family ELSE source_policy_capture_identity.parser_family END
 WHERE source_policy_capture_identity.capture_session_key=EXCLUDED.capture_session_key
 AND source_policy_capture_identity.storage_kind=EXCLUDED.storage_kind
 AND source_policy_capture_identity.parent_path=EXCLUDED.parent_path
 AND source_policy_capture_identity.parent_file_id=EXCLUDED.parent_file_id
 AND (source_policy_capture_identity.parser_family='' OR source_policy_capture_identity.parser_family=EXCLUDED.parser_family)`, device, ref.Path, ref.FileID, c.Session, c.Storage, c.ParentPath, c.ParentFileID, c.ParserFamily)
	if err == nil && tag.RowsAffected() != 1 {
		return policyIdentityConflict()
	}
	return err
}

// recovered is ephemeral trust from the caller's stored provenance check.
// It cannot relax an existing owner or capture binding.
func bindPolicySourceOwner(ctx context.Context, tx pgx.Tx, device, agent, native string, ref syncproto.PolicySource, recovered bool) error {
	physical := syncproto.SourceRef{Path: ref.Path, FileID: ref.FileID}
	p, exists, err := loadPolicyIdentity(ctx, tx, device, physical)
	if err != nil {
		return err
	}
	if exists && (p.Agent != agent || p.Owner != native) {
		return policyIdentityConflict()
	}
	if !exists {
		p = policySourceIdentity{Agent: agent, Owner: native}
	}
	src, observed, err := observedPolicySource(ctx, tx, device, physical)
	if err != nil {
		return err
	}
	if !observed && src.Agent != "" && src.Agent != agent {
		return policyIdentityConflict()
	}
	if recovered && (!observed || src.StorageKind != "cass_export") {
		return policyIdentityConflict()
	}
	if observed {
		// A verified physical CASS alias is frozen as capture identity. It remains
		// distinct from the canonical native owner and never provides folder grants.
		if recovered && p.Capture == nil {
			c, err := policyCanonicalCapture(ctx, tx, device, p, src)
			if err != nil {
				return err
			}
			p.Capture = &c
		}
		if err := checkPolicyCapture(ctx, tx, device, p, src); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO source_policy_identity(device_id,path,file_id,owner_agent,owner_session_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, device, ref.Path, ref.FileID, agent, native)
	if err != nil {
		return err
	}
	// Verify even if a competing binding won. Device gates serialize normal
	// callers; this check also prevents an accidental unguarded conflict grant.
	bound, _, err := loadPolicyIdentity(ctx, tx, device, physical)
	if err != nil {
		return err
	}
	if bound.Agent != agent || bound.Owner != native {
		return policyIdentityConflict()
	}
	if observed {
		capture, err := policyCanonicalCapture(ctx, tx, device, p, src)
		if err != nil {
			return err
		}
		return insertPolicyCapture(ctx, tx, device, physical, capture)
	}
	return nil
}

// A companion may create an empty parent row before the main upload. That
// row proves physical linkage, not capture attributes. Only an entirely empty
// descriptor without generations or messages may be refined on first capture.
func observedPolicySource(ctx context.Context, q policyQuerier, device string, physical syncproto.SourceRef) (syncproto.Source, bool, error) {
	src := syncproto.Source{Path: physical.Path, FileID: physical.FileID}
	var parentPath, parentFile string
	var captured bool
	err := q.QueryRow(ctx, `SELECT s.agent,COALESCE(s.session_key,''),s.storage_kind,COALESCE(s.parser,''),COALESCE(s.parent_path,parent.path,''),COALESCE(s.parent_file_id,parent.file_id,''),
 EXISTS(SELECT 1 FROM generations g WHERE g.source_id=s.id) OR EXISTS(SELECT 1 FROM messages m WHERE m.source_id=s.id)
 FROM sources s LEFT JOIN sources parent ON parent.id=s.parent_source_id AND parent.device_id=s.device_id WHERE s.device_id=$1 AND s.path=$2 AND s.file_id=$3`, device, physical.Path, physical.FileID).Scan(&src.Agent, &src.SessionKey, &src.StorageKind, &src.Parser, &parentPath, &parentFile, &captured)
	if errors.Is(err, pgx.ErrNoRows) {
		return src, false, nil
	}
	if err != nil {
		return src, false, err
	}
	if parentPath != "" {
		src.Parent = &syncproto.SourceRef{Path: parentPath, FileID: parentFile}
	}
	placeholder := !captured && src.SessionKey == "" && src.StorageKind == "" && src.Parser == "" && parentPath == "" && parentFile == ""
	return src, !placeholder, nil
}

func checkPolicySourceIdentity(ctx context.Context, q policyQuerier, device string, src syncproto.Source) error {
	physical := syncproto.SourceRef{Path: src.Path, FileID: src.FileID}
	p, exists, err := loadPolicyIdentity(ctx, q, device, physical)
	if err != nil {
		return err
	}
	if !exists && src.Parent != nil {
		parent, protected, err := loadPolicyIdentity(ctx, q, device, *src.Parent)
		if err != nil {
			return err
		}
		if !protected {
			return nil
		}
		p = policySourceIdentity{Agent: parent.Agent, Owner: parent.Owner}
		// An unlisted companion cannot repurpose an already captured main file.
		old, observed, err := observedPolicySource(ctx, q, device, physical)
		if err != nil {
			return err
		}
		if !observed && old.Agent != "" && old.Agent != p.Agent {
			return policyIdentityConflict()
		}
		if observed {
			if err := checkPolicyCapture(ctx, q, device, p, old); err != nil {
				return err
			}
			c, err := policyCanonicalCapture(ctx, q, device, p, old)
			if err != nil {
				return err
			}
			p.Capture = &c
		}
	} else if !exists {
		return nil
	}
	return checkPolicyCapture(ctx, q, device, p, src)
}

// Captured identity is bound under the shared device gate, alongside source
// commit. Generation changes do not produce a new physical identity key.
func bindPolicyCaptureIdentity(ctx context.Context, tx pgx.Tx, device string, src syncproto.Source) error {
	physical := syncproto.SourceRef{Path: src.Path, FileID: src.FileID}
	p, exists, err := loadPolicyIdentity(ctx, tx, device, physical)
	if err != nil {
		return err
	}
	if !exists && src.Parent != nil {
		parent, protected, err := loadPolicyIdentity(ctx, tx, device, *src.Parent)
		if err != nil {
			return err
		}
		if !protected {
			return nil
		}
		if err := bindPolicySourceOwner(ctx, tx, device, parent.Agent, parent.Owner, syncproto.PolicySource{Path: src.Path, FileID: src.FileID}, false); err != nil {
			return err
		}
		p, exists, err = loadPolicyIdentity(ctx, tx, device, physical)
		if err != nil {
			return err
		}
	}
	if !exists {
		return nil
	}
	if src.Parent != nil {
		agent, owner, err := policyCaptureParent(ctx, tx, device, p, src)
		if err != nil {
			return err
		}
		if agent != p.Agent || owner != p.Owner {
			return policyIdentityConflict()
		}
		if err := bindPolicySourceOwner(ctx, tx, device, agent, owner, syncproto.PolicySource{Path: src.Parent.Path, FileID: src.Parent.FileID}, false); err != nil {
			return err
		}
	}
	if err := checkPolicyCapture(ctx, tx, device, p, src); err != nil {
		return err
	}
	capture, err := policyCanonicalCapture(ctx, tx, device, p, src)
	if err != nil {
		return err
	}
	return insertPolicyCapture(ctx, tx, device, physical, capture)
}
