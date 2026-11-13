package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/api"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/output"
)

// categories maps the coarse, user-facing category to the server's model
// classifications — the same grouping the MCP list_models tool uses.
var categories = []struct {
	name            string
	classifications []string
}{
	{"image", []string{"text_to_image", "image_to_image"}},
	{"video", []string{"text_to_video", "image_to_video", "video_to_video"}},
	{"speech", []string{"audio"}},
}

func categoryOf(classifications []string) string {
	for _, cat := range categories {
		for _, want := range cat.classifications {
			for _, have := range classifications {
				if have == want {
					return cat.name
				}
			}
		}
	}
	return ""
}

// modelSummary matches the MCP list_models output.
type modelSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Provider    string   `json:"provider"`
	Series      string   `json:"series,omitempty"`
	Description string   `json:"description,omitempty"`
	InputTypes  []string `json:"input_types,omitempty"`
	Lifecycle   string   `json:"lifecycle"`
}

func (a *App) newModelsCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Browse the model catalog and model parameters",
		Args:  positional(noArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	var category string
	list := &cobra.Command{
		Use:   "list",
		Short: "List generation models",
		Example: `  apipod models list --category video
  apipod models list --json | jq -r '.models[].id'`,
		Args: positional(noArgs),
		RunE: a.run(flags, func(rt *runtime, _ []string) error { return rt.listModels(category) }),
	}
	list.Flags().StringVar(&category, "category", "", "filter by category: image, video or speech")

	params := &cobra.Command{
		Use:   "params <model>",
		Short: "Show the parameters a model accepts",
		Long: `Show a model's request parameters: names, types, defaults and allowed values.

Parameters without a dedicated flag are passed to "apipod image" / "apipod
video" with -x name=value, and to "apipod cost" with -d name=value.`,
		Args: positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error { return rt.modelParams(args[0]) }),
	}

	cmd.AddCommand(list, params)
	return cmd
}

func (rt *runtime) listModels(category string) error {
	var filters []string
	if category != "" {
		for _, cat := range categories {
			if cat.name == category {
				filters = cat.classifications
			}
		}
		if filters == nil {
			return usagef("--category must be one of: image, video, speech")
		}
	} else {
		filters = []string{""} // one unfiltered request
	}

	client, err := rt.publicClient()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	summaries := []modelSummary{}
	for _, classification := range filters {
		models, err := client.ListModels(rt.cmd.Context(), classification)
		if err != nil {
			return err
		}
		for _, m := range models {
			cat := categoryOf(m.Classifications)
			// Models that are only LLMs or music have no category here.
			if cat == "" || (category != "" && cat != category) || m.Lifecycle == "offline" || seen[m.ModelID] {
				continue
			}
			seen[m.ModelID] = true
			name := m.ModelID
			if m.DisplayName != nil && strings.TrimSpace(*m.DisplayName) != "" {
				name = strings.TrimSpace(*m.DisplayName)
			}
			summaries = append(summaries, modelSummary{
				ID:          m.ModelID,
				Name:        name,
				Category:    cat,
				Provider:    m.ProviderID,
				Series:      m.SeriesID,
				Description: output.Truncate(strings.TrimSpace(m.Description), 280),
				InputTypes:  m.InputTypes,
				Lifecycle:   m.Lifecycle,
			})
		}
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].Category != summaries[j].Category {
			return summaries[i].Category < summaries[j].Category
		}
		return summaries[i].ID < summaries[j].ID
	})

	return rt.p.Result(map[string]any{"models": summaries}, func(w io.Writer) {
		if len(summaries) == 0 {
			fmt.Fprintln(w, "No models found.")
			return
		}
		rows := make([][]string, len(summaries))
		for i, s := range summaries {
			rows[i] = []string{s.ID, s.Category, s.Provider, strings.Join(s.InputTypes, ","), s.Lifecycle, s.Name}
		}
		rt.p.Table(w, []string{"ID", "CATEGORY", "PROVIDER", "INPUTS", "LIFECYCLE", "NAME"}, rows)
	})
}

// paramInfo is one row of `models params`.
type paramInfo struct {
	Name        string
	Type        string
	Required    bool
	Default     string
	Allowed     string
	Description string
}

func (rt *runtime) modelParams(model string) error {
	client, err := rt.publicClient()
	if err != nil {
		return err
	}
	schema, err := client.GetModelSchema(rt.cmd.Context(), model)
	if err != nil {
		if api.IsNotFound(err) {
			return exitcode.New(exitcode.NotFound, "no published parameters for model %q. Check the id with `apipod models list`", model)
		}
		return err
	}
	input := schema.Input()
	result := map[string]any{"model": model}
	if input != nil {
		result["input_schema"] = input
	} else {
		result["note"] = "This model publishes no input schema; use the dedicated flags only."
	}
	return rt.p.Result(result, func(w io.Writer) {
		params := describeParams(input)
		if len(params) == 0 {
			fmt.Fprintln(w, "This model publishes no parameters; use the dedicated flags only.")
			return
		}
		rows := make([][]string, len(params))
		for i, p := range params {
			required := ""
			if p.Required {
				required = "yes"
			}
			rows[i] = []string{p.Name, p.Type, required, p.Default, p.Allowed, output.Truncate(p.Description, 70)}
		}
		rt.p.Table(w, []string{"PARAM", "TYPE", "REQUIRED", "DEFAULT", "ALLOWED", "DESCRIPTION"}, rows)
	})
}

// describeParams flattens a JSON Schema object into table rows, in the
// schema's declared display order when it has one.
func describeParams(input map[string]any) []paramInfo {
	props, _ := input["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	required := map[string]bool{}
	for _, name := range stringList(input["required"]) {
		required[name] = true
	}

	names := make([]string, 0, len(props))
	listed := map[string]bool{}
	for _, name := range stringList(input["x-apipod-order-properties"]) {
		if _, ok := props[name]; ok && !listed[name] {
			names = append(names, name)
			listed[name] = true
		}
	}
	rest := make([]string, 0, len(props))
	for name := range props {
		if !listed[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	names = append(names, rest...)

	out := make([]paramInfo, 0, len(names))
	for _, name := range names {
		// model comes from the positional/-m argument, not from -x.
		if name == "model" {
			continue
		}
		prop, _ := props[name].(map[string]any)
		info := paramInfo{Name: name, Required: required[name]}
		info.Type, _ = prop["type"].(string)
		info.Description, _ = prop["description"].(string)
		if def, ok := prop["default"]; ok && def != "" && def != nil {
			info.Default = compactJSON(def)
		}
		info.Allowed = allowedValues(prop)
		out = append(out, info)
	}
	return out
}

func allowedValues(prop map[string]any) string {
	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		parts := make([]string, len(enum))
		for i, v := range enum {
			parts[i] = compactJSON(v)
		}
		return strings.Join(parts, " | ")
	}
	lo, hasLo := prop["minimum"]
	hi, hasHi := prop["maximum"]
	switch {
	case hasLo && hasHi:
		return compactJSON(lo) + " to " + compactJSON(hi)
	case hasLo:
		return "min " + compactJSON(lo)
	case hasHi:
		return "max " + compactJSON(hi)
	}
	return ""
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// compactJSON renders a schema value without quotes around plain strings.
func compactJSON(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
