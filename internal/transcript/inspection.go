package transcript

// ExtractionSummary covers current Claude and Codex JSONL sources. An absent
// checkpoint is unassessed, not evidence of a clean extraction.
type ExtractionSummary struct {
	AssessedSources   int64                     `json:"assessed_sources"`
	UnassessedSources int64                     `json:"unassessed_sources"`
	AffectedSources   int64                     `json:"affected_sources"`
	WarningSources    int64                     `json:"warning_sources"`
	InfoSources       int64                     `json:"info_sources"`
	Counts            map[DiagnosticCode]uint64 `json:"counts"`
	Affected          []SourceReference         `json:"affected"`
}
type SourceReference struct {
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
	Agent    Agent  `json:"agent"`
}
type SourceDiagnostics struct {
	SourceReference
	DeviceID            string                `json:"device_id"`
	Extraction          *ExtractionCheckpoint `json:"extraction"`
	MissingCompanions   int64                 `json:"missing_companions"`
	TruncatedCompanions int64                 `json:"truncated_companions"`
}
