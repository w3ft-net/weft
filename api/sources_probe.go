package api

import (
	"encoding/json"
	weftsdk "github.com/w3ft-net/weft-go"
	"net/http"

	"github.com/graybeardcorp/weft/ingest"
)

const maxProbeMarks = 100

// handleProbeSource finds an extraction rule for one value from the user's
// marks on the source's stored sample and shows what it does to every line.
// POST /api/sources/{namespace}/{app}/probe
//
// It is stateless and writes nothing: the wizard calls it repeatedly as the
// user highlights values and flags lines, and only the final configure call
// persists a schema. The sample is read from the source row rather than sent
// by the client, so the lines the user sees are the lines extracted from.
func (s *Server) handleProbeSource(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	namespace := r.PathValue("namespace")
	app := r.PathValue("app")

	var req struct {
		Field    ingest.ProbeField      `json:"field"`
		Base     ingest.ProbeBase       `json:"base"`
		Marks    []ingest.ProbeMark     `json:"marks"`
		Previous *ingest.ProbeExtractor `json:"previous"`
		// UseDetected starts from the source's detected candidate for the
		// field (as Previous) when the client has none of its own.
		UseDetected bool `json:"use_detected"`
		// Propose asks for the model's first guess when nothing has been marked.
		Propose bool `json:"propose"`
		// DurationUnit is what a bare duration number is counted in.
		DurationUnit string `json:"duration_unit"`
		// DetectedIndex picks which detected candidate (0-based) to start from,
		// so a client can offer them one after another.
		DetectedIndex int `json:"detected_index"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Marks) > maxProbeMarks {
		http.Error(w, "too many marks", http.StatusBadRequest)
		return
	}

	lg, err := s.store.FindLogGroup(session.Org(), namespace, app)
	if err != nil {
		weftsdk.Infof(r.Context(), "probe source %s/%s: FindLogGroup: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if lg == nil {
		http.Error(w, "no source found", http.StatusNotFound)
		return
	}

	var lines []string
	if lg.Sample != "" {
		_ = json.Unmarshal([]byte(lg.Sample), &lines)
	}
	if len(lines) == 0 {
		http.Error(w, "source has no sample yet", http.StatusConflict)
		return
	}

	detectedTotal := 0
	if req.UseDetected && req.Previous == nil {
		var det ingest.Detection
		var all []*ingest.ProbeExtractor
		if lg.Detection != "" && json.Unmarshal([]byte(lg.Detection), &det) == nil {
			all = ingest.DetectedExtractors(&det, req.Field)
		}
		detectedTotal = len(all)
		if req.DetectedIndex >= 0 && req.DetectedIndex < len(all) {
			req.Previous = all[req.DetectedIndex]
		}
		if req.Previous == nil && len(req.Marks) == 0 {
			// Nothing (more) was detected for this field: say so, not an error.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"extractor": nil, "results": []any{}, "matched": 0, "total": len(lines),
				"message": "nothing detected for this field", "detected_total": detectedTotal,
			})
			return
		}
	}

	probeReq := ingest.ProbeRequest{
		Field: req.Field, Base: req.Base, Lines: lines, Marks: req.Marks, Previous: req.Previous,
		Propose: req.Propose, DurationUnit: req.DurationUnit,
	}
	// If the classifier can propose extractors, let it help when the
	// built-in search finds none. Its proposals are verified like any other.
	if p, ok := s.classifier.(ingest.ExtractorProposer); ok {
		probeReq.Proposer = p
	}
	res, err := ingest.ProbeContext(r.Context(), probeReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	description := ""
	if res.Extractor != nil {
		description = res.Extractor.Describe()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		*ingest.ProbeResult
		Description   string         `json:"description"`
		SchemaPatch   map[string]any `json:"schema_patch"`
		DetectedTotal int            `json:"detected_total,omitempty"`
	}{res, description, res.SchemaPatch(req.Field), detectedTotal})
}
