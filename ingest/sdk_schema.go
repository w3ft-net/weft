package ingest

// SDKSchema returns the schema YAML for a source written by the weft SDK:
// nothing to extract, because the line header supplies the timestamp, level,
// trace and span values. format is the body format the SDK declared, "json"
// or "text"; anything else reads as text.
func SDKSchema(service, format string) (string, error) {
	if format != "json" {
		format = "text"
	}
	s := Schema{
		Service:        service,
		Envelope:       "none",
		Format:         format,
		TimestampField: "none",
		SDK:            true,
	}
	return s.ToYAML()
}
