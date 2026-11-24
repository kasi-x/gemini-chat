package gemini

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const modelHeaderKey = "x-goog-ext-525001261-jspb"

// AvailableModel mirrors gemini_webapi.types.AvailableModel.
type AvailableModel struct {
	ModelID      string
	ModelName    string
	DisplayName  string
	Description  string
	Capacity     int
	CapacityFld  int // capacity_field
	ModelNumber  int
	IsAvailable  bool
	Aliases      []string
	modelIDLower string
}

var versionRe = regexp.MustCompile(`(\d+)(?:\.\d+)?`)
var modelPrefixRe = regexp.MustCompile(`^gemini-(?:\d+(?:\.\d+)?-)`)

// ModelHeader returns the JSPB header payload string for this model, as in
// constants.build_model_header. capacity_tail is CapacityFld.
func (m *AvailableModel) ModelHeaderJSON() string {
	tail := m.CapacityFld
	return fmt.Sprintf(`[1,null,null,null,"%s",null,null,0,[4,5,6,8],null,null,%d,null,null,%d]`,
		m.ModelID, tail, m.ModelNumber)
}

// ExtraHeaders mirrors the companion headers build_model_header emits.
func (m *AvailableModel) ExtraHeaders() map[string]string {
	return map[string]string{
		"x-goog-ext-73010989-jspb": "[0]",
		"x-goog-ext-73010990-jspb": "[0,0,0]",
	}
}

// computeCapacity ports AvailableModel.compute_capacity.
func computeCapacity(tierFlags, capabilityFlags []any) (int, int) {
	has := func(list []any, v int) bool {
		for _, x := range list {
			if f, ok := x.(float64); ok && int(f) == v {
				return true
			}
		}
		return false
	}
	if has(tierFlags, 21) {
		return 1, 13
	}
	if has(tierFlags, 22) {
		return 2, 13
	}
	if has(capabilityFlags, 115) {
		return 4, 12
	}
	if has(tierFlags, 16) || has(capabilityFlags, 106) {
		return 3, 12
	}
	if has(tierFlags, 8) || has(capabilityFlags, 19) {
		return 2, 12
	}
	return 1, 12
}

// deriveNameAndAliases ports AvailableModel._derive_name_and_aliases.
func deriveNameAndAliases(modelID, categoryName, displayName string) (string, []string) {
	aliases := map[string]bool{}
	if modelID != "" {
		aliases[strings.ToLower(modelID)] = true
	}
	catClean := strings.TrimSpace(categoryName)
	dispClean := strings.TrimSpace(displayName)

	majorVer := ""
	if m := versionRe.FindStringSubmatch(dispClean); m != nil {
		majorVer = m[1]
	}
	if catClean != "" {
		catSlug := strings.ToLower(strings.ReplaceAll(catClean, " ", "-"))
		aliases[strings.ToLower(catClean)] = true
		aliases[catSlug] = true
		aliases["gemini-"+catSlug] = true
		if majorVer != "" {
			aliases["gemini-"+majorVer+"-"+catSlug] = true
		}
	}
	if dispClean != "" {
		dispSlug := strings.ToLower(strings.ReplaceAll(dispClean, " ", "-"))
		aliases[strings.ToLower(dispClean)] = true
		aliases[dispSlug] = true
		aliases["gemini-"+dispSlug] = true
	}

	var primary string
	if catClean != "" {
		primary = "gemini-" + strings.ToLower(strings.ReplaceAll(catClean, " ", "-"))
	} else if dispClean != "" {
		primary = "gemini-" + strings.ToLower(strings.ReplaceAll(dispClean, " ", "-"))
	} else {
		primary = "gemini-" + modelID
	}
	aliases[primary] = true

	out := make([]string, 0, len(aliases))
	for a := range aliases {
		out = append(out, a)
	}
	sort.Strings(out)
	return primary, out
}

// modelFromRPC ports AvailableModel.from_rpc for one model_data list item.
// unavailable marks models a guest session may not select.
func modelFromRPC(modelData any, capacity, capacityField int, unavailable bool) *AvailableModel {
	arr, ok := modelData.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	modelID, _ := getNestedValue(arr, []any{0}, "").(string)
	if modelID == "" {
		return nil
	}

	categoryName := asString(arr, 1)
	if categoryName == "" {
		categoryName = asString(arr, 10)
	}
	displayName := asString(arr, 11)
	if displayName == "" {
		displayName = asString(arr, 19)
	}
	if displayName == "" {
		displayName = asString(arr, 1)
	}
	description := asString(arr, 12)
	if description == "" {
		description = asString(arr, 2)
	}

	modelNumber := 1
	if n, ok := getNestedValue(arr, []any{17}, nil).(float64); ok {
		modelNumber = int(n)
	} else if n, ok := getNestedValue(arr, []any{9}, nil).(float64); ok {
		modelNumber = int(n)
	}

	modelName, aliases := deriveNameAndAliases(modelID, categoryName, displayName)
	disp := displayName
	if disp == "" {
		disp = categoryName
	}
	if disp == "" {
		disp = modelID
	}
	return &AvailableModel{
		ModelID:      modelID,
		ModelName:    modelName,
		DisplayName:  disp,
		Description:  description,
		Capacity:     capacity,
		CapacityFld:  capacityField,
		ModelNumber:  modelNumber,
		IsAvailable:  !unavailable,
		Aliases:      aliases,
		modelIDLower: strings.ToLower(modelID),
	}
}

// resolveModelByName ports GeminiClient._resolve_model_by_name.
func resolveModelByName(registry map[string]*AvailableModel, name string) (*AvailableModel, error) {
	if m, ok := registry[name]; ok {
		return m, nil
	}
	target := strings.ToLower(strings.TrimSpace(name))

	// 1st priority: exact match on model_id, model_name or display_name.
	for _, m := range registry {
		if strings.ToLower(m.ModelName) == target ||
			strings.ToLower(m.DisplayName) == target ||
			m.modelIDLower == target {
			return m, nil
		}
	}
	// 2nd priority: alias match.
	for _, m := range registry {
		for _, a := range m.Aliases {
			if a == target {
				return m, nil
			}
		}
	}
	// 3rd priority: version-agnostic normalized match.
	normTarget := strings.ReplaceAll(modelPrefixRe.ReplaceAllString(target, ""), "_", "-")
	for _, m := range registry {
		normModel := strings.ReplaceAll(modelPrefixRe.ReplaceAllString(strings.ToLower(m.ModelName), ""), "_", "-")
		if normTarget == normModel {
			return m, nil
		}
		for _, part := range strings.Split(normModel, "-") {
			if normTarget == part {
				return m, nil
			}
		}
	}

	names := make([]string, 0, len(registry))
	for _, m := range registry {
		names = append(names, m.ModelName)
	}
	return nil, fmt.Errorf("Unknown model name: '%s'. Available registered models: %s",
		name, strings.Join(names, ", "))
}
