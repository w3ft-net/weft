package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/cmd/weft/setup"
	"github.com/w3ft-net/weft/cmd/weft/ui"
	"github.com/w3ft-net/weft/ingest"
	"github.com/w3ft-net/weft/shell"
)

func sourceCmd(e Env) *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Manage log sources (add, list, remove, pending, configure, resample, migrate, schema)"}
	cmd.AddCommand(
		sourceAddCmd(e), sourceListCmd(e), sourceRemoveCmd(e), sourcePendingCmd(e),
		sourceConfigureCmd(e), sourceResampleCmd(e), sourceMigrateCmd(e), sourceSchemaCmd(e),
	)
	return cmd
}

// target is the positional ns/svc every per-source command takes.
func target(arg string) (namespace, service string, err error) { return parseTarget(arg) }

// A source is registered as a push-based CloudWatch source: the AWS
// log-group to service mapping the CloudWatch adapter routes against, created
// pending. There is no live sample to pull at creation time (the Firehose
// stream has not been deployed yet), so this registers the mapping and
// `weft source configure` finishes onboarding once real events have
// accumulated a sample.
func sourceAddCmd(e Env) *cobra.Command {
	var logGroup, region string
	cmd := &cobra.Command{
		Use: "add NS/SVC --log-group GROUP", Short: "Register a CloudWatch log group as a source", Args: cobra.ExactArgs(1),
		RunE: runE(func(_ *cobra.Command, args []string) error {
			namespace, service, err := target(args[0])
			if err != nil {
				return err
			}
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			if err := client.AddKinesisSource(shell.AddKinesisSourceRequest{
				Namespace: namespace, Service: service, LogGroup: logGroup, Region: region,
			}); err != nil {
				return fmt.Errorf("add source: %v", err)
			}
			ui.Successf("Added pending source: %s/%s → %s (kinesis push).\n"+
				"Waiting for the Firehose stream to deliver events and accumulate a sample —\n"+
				"then run `weft source configure %s/%s` to approve a schema.\n",
				namespace, service, logGroup, namespace, service)
			return nil
		}),
	}
	cmd.Flags().StringVar(&logGroup, "log-group", "", "The CloudWatch log group")
	cmd.MarkFlagRequired("log-group")
	cmd.Flags().StringVar(&region, "region", "us-east-2", "The log group's AWS region")
	return cmd
}

func sourceListCmd(e Env) *cobra.Command {
	return &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List sources", Args: cobra.NoArgs,
		RunE: runE(func(cmd *cobra.Command, _ []string) error {
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			sources, err := client.ListSources()
			if err != nil {
				return fmt.Errorf("list sources: %v", err)
			}
			if len(sources) == 0 {
				ui.Hintln("No sources.")
				return nil
			}
			// SOURCE is the underlying origin: the CloudWatch log group for
			// cloudwatch sources, or the agent-reported path for weftd.
			// Region/last-poll are CloudWatch-poller concepts and meaningless for
			// pushed weftd sources, so `source schema <ns/svc>` has the per-source
			// detail.
			rows := make([][]string, 0, len(sources))
			for _, s := range sources {
				rows = append(rows, []string{s.Namespace, s.Service, statusText(s), s.SourceType, agentsText(s), s.LogGroup})
			}
			ui.WriteTable(cmd.OutOrStdout(), ui.Out(), "", []ui.Column{
				ui.MutedCol("NAMESPACE", 15), ui.NameCol("SERVICE", 20), ui.StatusCol("STATUS", 17),
				ui.MutedCol("TYPE", 10),
				{Title: "AGENTS", Width: 26, Style: func(s ui.Styles, _ string) lipgloss.Style { return s.Count }},
				{Title: "SOURCE"},
			}, rows)
			return nil
		}),
	}
}

// statusText is the STATUS column: the source's status, or what its resample
// is up to, since a source being re-sampled stays "configured" while it keeps
// ingesting.
func statusText(s shell.SourceInfo) string {
	switch r := s.Resample; {
	case r == nil:
		return s.Status
	case r.Ready:
		return "resampled"
	default:
		return fmt.Sprintf("resampling %d/%d", r.Collected, r.Target)
	}
}

// agentsText is the AGENTS column: the weftd versions reading a source and how
// many hosts, "unknown" for a weftd source no agent has reported for, and "-"
// for sources that have no agent (they are pushed, and parsed by the API).
func agentsText(s shell.SourceInfo) string {
	if s.SourceType != "weftd" {
		return "-"
	}
	if s.Agents == nil || s.Agents.Count == 0 {
		return "unknown"
	}
	hosts := fmt.Sprintf("%d hosts", s.Agents.Count)
	if s.Agents.Count == 1 {
		hosts = "1 host"
	}
	if s.Agents.Oldest == s.Agents.Newest {
		return fmt.Sprintf("%s (%s)", s.Agents.Oldest, hosts)
	}
	return fmt.Sprintf("%s-%s (%s)", s.Agents.Oldest, s.Agents.Newest, hosts)
}

func sourceRemoveCmd(e Env) *cobra.Command {
	return &cobra.Command{
		Use: "remove NS/SVC", Aliases: []string{"rm"}, Short: "Remove a source", Args: cobra.ExactArgs(1),
		RunE: runE(func(_ *cobra.Command, args []string) error {
			namespace, app, err := target(args[0])
			if err != nil {
				return err
			}
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			if err := client.RemoveSource(namespace, app); err != nil {
				return fmt.Errorf("remove source: %v", err)
			}
			ui.Successf("Removed source: %s/%s\n", namespace, app)
			return nil
		}),
	}
}

func sourcePendingCmd(e Env) *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use: "pending [NS]", Short: "Show sources that are collecting a sample or waiting for a schema", Args: cobra.MaximumNArgs(1),
		RunE: runE(func(cmd *cobra.Command, args []string) error {
			var namespace string
			if len(args) == 1 {
				var err error
				if namespace, err = namespaceArg(args[0]); err != nil {
					return err
				}
			}
			remote, err := e.Coordinator()
			if err != nil {
				return err
			}
			if watch {
				return watchPending(remote, namespace)
			}
			sources, err := remote.ListPendingSources(namespace)
			if err != nil {
				return fmt.Errorf("list pending: %v", err)
			}
			if len(sources) == 0 {
				ui.Hintln("No pending sources.")
				return nil
			}
			renderPending(cmd.OutOrStdout(), sources, now(), terminalWidth())
			return nil
		}),
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Refresh until every sample is full")
	return cmd
}

func terminalWidth() int {
	if w, _, _ := term.GetSize(int(os.Stdout.Fd())); w > 0 {
		return w
	}
	return 100
}

func sourceResampleCmd(e Env) *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use: "resample NS/SVC", Short: "Collect a fresh sample of a source", Args: cobra.ExactArgs(1),
		Long: "Asks for a fresh sample of a pending or configured source, for when the lines sampled so far\n" +
			"(process startup, say) don't show the values a schema needs. A configured source keeps\n" +
			"ingesting on its current schema; the new sample shows up for review once a full one has\n" +
			"accumulated.",
		RunE: runE(func(_ *cobra.Command, args []string) error {
			namespace, service, err := target(args[0])
			if err != nil {
				return err
			}
			remote, err := e.Coordinator()
			if err != nil {
				return err
			}
			if err := remote.ResampleSource(namespace, service, lines); err != nil {
				return fmt.Errorf("resample: %v", err)
			}
			ui.Successf("Resample requested for %s/%s. A fresh sample of new lines will be collected; a configured source keeps ingesting on its current schema meanwhile.\n", namespace, service)
			ui.Hintf("Track progress with: weft source pending --watch\n")
			return nil
		}),
	}
	cmd.Flags().IntVar(&lines, "lines", 0, "How many lines to collect (default: the server's)")
	return cmd
}

func sourceSchemaCmd(e Env) *cobra.Command {
	var asYAML bool
	cmd := &cobra.Command{
		Use: "schema NS/SVC", Short: "Show a source's schema, or how far its sample is", Args: cobra.ExactArgs(1),
		RunE: runE(func(cmd *cobra.Command, args []string) error {
			namespace, service, err := target(args[0])
			if err != nil {
				return err
			}
			remote, err := e.Coordinator()
			if err != nil {
				return err
			}
			sch, err := remote.GetSourceSchema(namespace, service)
			if err != nil {
				return fmt.Errorf("schema: %v", err)
			}
			w, _, _ := term.GetSize(int(os.Stdout.Fd()))
			printSourceSchema(cmd.OutOrStdout(), sch, namespace, service, asYAML, w)
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asYAML, "yaml", false, "Print the raw schema YAML (it round-trips into `source configure --file`)")
	return cmd
}

// printSourceSchema renders a source's schema state. --yaml prints the raw
// schema YAML exactly (it round-trips into `source configure --file`);
// otherwise a configured source shows its schema applied to recent lines.
func printSourceSchema(w io.Writer, sch *shell.SourceSchema, namespace, service string, asYAML bool, width int) {
	switch sch.Status {
	case "configured":
		if sch.ResampleRequested {
			ui.Errorf("# resample in progress: %d/%d lines collected\n", sch.SampleCollected, sch.SampleTarget)
		}
		if sch.SDKNote != "" && !asYAML {
			fmt.Fprint(w, ui.Paint(ui.Out().Muted, sch.SDKNote+"\n"))
		}
		switch {
		case asYAML:
			fmt.Fprint(w, sch.SchemaYAML)
			if !strings.HasSuffix(sch.SchemaYAML, "\n") {
				fmt.Fprintln(w)
			}
		case sch.Preview != nil && len(sch.Preview.Lines) > 0:
			fmt.Fprint(w, setup.RenderPreview(sch.Preview, width))
		case sch.PreviewNote != "":
			fmt.Fprint(w, ui.Paint(ui.Out().Muted, sch.PreviewNote+"\n"))
		default:
			fmt.Fprint(w, ui.Paint(ui.Out().Muted, "No recent lines to preview. Use --yaml for the raw schema.\n"))
		}
	case "sampling":
		fmt.Fprintf(w, "%s/%s hasn't emitted enough lines to sample yet (%d/%d).\n",
			namespace, service, sch.SampleCollected, sch.SampleTarget)
	default: // pending
		fmt.Fprintf(w, "%s/%s has a sample awaiting configuration (%d/%d lines). Configure it with: weft source configure %s/%s\n",
			namespace, service, sch.SampleCollected, sch.SampleTarget, namespace, service)
	}
}

func sourceConfigureCmd(e Env) *cobra.Command {
	var filePath string
	cmd := &cobra.Command{
		Use: "configure NS/SVC [--file schema.yaml]", Short: "Approve a schema for a pending source", Args: cobra.ExactArgs(1),
		Long: "Walks a source's sample through the setup wizard, or applies a schema document\n" +
			"non-interactively: from --file, or from stdin when it is not a terminal.",
		RunE: runE(func(_ *cobra.Command, args []string) error {
			namespace, service, err := target(args[0])
			if err != nil {
				return err
			}
			remote, err := e.Coordinator()
			if err != nil {
				return err
			}
			return configureSource(remote, namespace, service, filePath)
		}),
	}
	cmd.Flags().StringVar(&filePath, "file", "", "Apply this schema YAML instead of running the wizard")
	return cmd
}

func configureSource(remote *shell.HTTPCoordinator, namespace, service, filePath string) error {
	// A source is configurable when it has a sample to drive the wizard: a
	// pending first-time sample, or, for an already-configured source, a fresh
	// sample left by a completed resample. Both arrive on the schema endpoint;
	// the configure endpoint itself accepts either status.
	sch, err := remote.GetSourceSchema(namespace, service)
	if err != nil {
		return fmt.Errorf("schema: %v", err)
	}
	switch sch.Status {
	case "sampling":
		return fmt.Errorf("%s/%s hasn't collected a full sample yet (%d/%d lines). Try again once it has.",
			namespace, service, sch.SampleCollected, sch.SampleTarget)
	case "configured":
		if sch.ResampleRequested {
			return fmt.Errorf("resample of %s/%s in progress (%d/%d lines) — re-run configure once it completes.",
				namespace, service, sch.SampleCollected, sch.SampleTarget)
		}
		if sch.SDKNote != "" {
			return fmt.Errorf("%s/%s is already configured. %s", namespace, service, sch.SDKNote)
		}
		if len(sch.Sample) == 0 || string(sch.Sample) == "[]" {
			return fmt.Errorf("%s/%s is already configured. To change its schema, collect a fresh sample first:\n    weft source resample %s/%s\nthen re-run configure once it lands.",
				namespace, service, namespace, service)
		}
	case "pending":
		// First-time configure; the sample is on the response. If it turns out to
		// be the wrong lines, `weft source resample` collects new ones.
	default:
		return fmt.Errorf("%s/%s: unexpected status %q", namespace, service, sch.Status)
	}

	// Non-interactive: --file, or stdin redirected/piped (not a terminal).
	// Checked in that order so --file works even when stdin happens to be a
	// non-terminal for unrelated reasons (e.g. run from another script).
	var schema *ingest.Schema
	if filePath != "" || !term.IsTerminal(int(os.Stdin.Fd())) {
		schema, err = loadSchemaNonInteractive(filePath)
		if err != nil {
			return err
		}
		// Service is advisory in the YAML (see ingest.Schema's doc comment): the
		// target on the command line is authoritative, same as the server does
		// when it builds the schema it persists.
		schema.Service = service
		if problems := schema.Validate(); len(problems) > 0 {
			ui.Errorln("schema validation failed:")
			for _, p := range problems {
				ui.Errorf("  - %s\n", p)
			}
			return errReported
		}
	} else {
		var lines []string
		if err := json.Unmarshal(sch.Sample, &lines); err != nil {
			return fmt.Errorf("parse sample: %v", err)
		}
		if len(lines) == 0 {
			return fmt.Errorf("no sample lines available for %s/%s", namespace, service)
		}
		// The server already computed detection for this sample (heuristic
		// envelope/JSON detection, plus LLM-classified timestamp/level/trace/span
		// candidates once classification completes); use it instead of
		// recomputing heuristic-only detection locally. Falls back to a local
		// recompute if Detection is missing rather than showing an empty wizard.
		var det *ingest.Detection
		if len(sch.Detection) > 0 {
			det = &ingest.Detection{}
			if err := json.Unmarshal(sch.Detection, det); err != nil {
				det = nil
			}
		}
		// Probes run on the server, against the stored sample, so the CLI gets
		// the same rule-building and model help as the browser.
		probe := func(req ingest.ProbeRequest) (*ingest.ProbeResult, error) {
			body := map[string]any{
				"field": req.Field, "base": req.Base, "marks": req.Marks, "previous": req.Previous,
				"propose": req.Propose, "duration_unit": req.DurationUnit,
			}
			var res ingest.ProbeResult
			if err := remote.ProbeSource(namespace, service, body, &res); err != nil {
				return nil, err
			}
			return &res, nil
		}
		schema, err = setup.RunSetupFromLines(namespace+"/"+service, lines, det, probe)
		if errors.Is(err, setup.ErrLargerSample) {
			// Nothing is configured from a walk that is waiting on more lines.
			if err := remote.ResampleSource(namespace, service, setup.LargerSampleLines); err != nil {
				return fmt.Errorf("resample: %v", err)
			}
			ui.Successf("Collecting %d fresh lines for %s/%s. Run weft source configure %s/%s again once they land.\n",
				setup.LargerSampleLines, namespace, service, namespace, service)
			ui.Hintf("Track progress with: weft source pending --watch\n")
			return nil
		}
		if err != nil {
			return fmt.Errorf("setup: %v", err)
		}
	}

	if err := remote.ConfigureSource(namespace, service, schemaConfigMap(schema)); err != nil {
		return fmt.Errorf("configure: %v", err)
	}
	ui.Successf("Source %s/%s configured.\n", namespace, service)
	return nil
}

// loadSchemaNonInteractive reads and parses a schema YAML document from path,
// or from stdin when path is empty: the `weft source configure ns/svc <
// schema.yaml` shape. It is the same ingest.Schema shape ToYAML and
// LoadSchemaFromBytes round-trip elsewhere (weft source schema --yaml prints
// exactly this format), so a schema pulled with `weft source schema` is valid
// input here unmodified.
func loadSchemaNonInteractive(path string) (*ingest.Schema, error) {
	var data []byte
	var err error
	if path != "" {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	} else {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read schema from stdin: %w", err)
		}
	}
	// An empty document parses to a blank schema that validates, and configuring
	// that replaces a working schema with nothing (stdin at /dev/null, a
	// truncated file). Refuse it rather than apply it.
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err == nil && len(doc) == 0 {
		return nil, fmt.Errorf("schema document is empty — refusing to configure a blank schema")
	}
	schema, err := ingest.LoadSchemaFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse schema YAML: %w", err)
	}
	return schema, nil
}

// schemaConfigMap builds the /configure request body from a schema, covering
// every field ingest.Schema and the server's handleConfigureSource both
// understand. Shared by the interactive wizard, the non-interactive (--file or
// stdin) path and migrate, so none silently drops a field another sends.
func schemaConfigMap(schema *ingest.Schema) map[string]any {
	return map[string]any{
		"envelope":               schema.Envelope,
		"format":                 schema.Format,
		"field_separator":        schema.FieldSeparator,
		"timestamp_field":        schema.TimestampField,
		"timestamp_format":       schema.TimestampFormat,
		"timestamp_fields":       schema.TimestampFields,
		"extract":                schema.Extract,
		"level_field":            schema.LevelField,
		"level_pattern":          schema.LevelPattern,
		"trace_id_field":         schema.TraceIDField,
		"trace_id_pattern":       schema.TraceIDPattern,
		"span_name_field":        schema.SpanNameField,
		"span_name_pattern":      schema.SpanNamePattern,
		"span_id_field":          schema.SpanIDField,
		"span_id_pattern":        schema.SpanIDPattern,
		"parent_span_id_field":   schema.ParentSpanIDField,
		"parent_span_id_pattern": schema.ParentSpanIDPattern,
		"duration_ns_field":      schema.DurationNsField,
		"duration_ns_pattern":    schema.DurationNsPattern,
		"duration_unit":          schema.DurationUnit,
		"json_start_field":       schema.JSONStartField,
		"expand_keys":            schema.ExpandKeys,
	}
}
