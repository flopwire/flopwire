WITH RECURSIVE owned AS (
 SELECT s.id FROM sources s WHERE s.device_id=$1 AND s.agent=$2 AND (
 s.session_key=$3 OR right(s.path,length($3)+6)=$3||'.jsonl' OR s.id=ANY($4::uuid[])
 OR EXISTS(SELECT 1 FROM conversations c WHERE c.source_id=s.id AND c.device_id=$1 AND c.agent=$2 AND c.session_id=$3)
 OR EXISTS(SELECT 1 FROM jsonb_array_elements($5::jsonb) ref WHERE s.path=ref->>'path' AND s.file_id=ref->>'file_id'))
 UNION SELECT child.id FROM sources child JOIN owned parent_id ON true JOIN sources parent ON parent.id=parent_id.id
 WHERE child.device_id=$1 AND child.agent=$2 AND (child.parent_source_id=parent.id OR (child.parent_path=parent.path AND (child.parent_file_id IS NULL OR child.parent_file_id=parent.file_id))))
 SELECT EXISTS(SELECT 1 FROM sources s WHERE s.id IN(SELECT id FROM owned) AND (EXISTS(SELECT 1 FROM generations g WHERE g.source_id=s.id AND g.size>0 AND NOT EXISTS(SELECT 1 FROM source_policy_placements b JOIN session_policy_placements p ON p.device_id=b.device_id AND p.agent=b.agent AND p.session_id=b.session_id WHERE b.device_id=s.device_id AND b.path=s.path AND b.file_id=s.file_id AND b.generation=g.generation AND p.evidence_scope='mapped'))
		 OR EXISTS(SELECT 1 FROM messages m WHERE m.source_id=s.id AND NOT EXISTS(SELECT 1 FROM source_policy_placements b JOIN session_policy_placements p ON p.device_id=b.device_id AND p.agent=b.agent AND p.session_id=b.session_id WHERE b.device_id=s.device_id AND b.path=s.path AND b.file_id=s.file_id AND b.generation=m.source_generation AND p.evidence_scope='mapped'))))